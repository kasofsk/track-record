package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/kasofsk/track-record/internal/api"
	"github.com/kasofsk/track-record/internal/client"
	"github.com/kasofsk/track-record/internal/inputs"
	"github.com/kasofsk/track-record/internal/key"
	"github.com/kasofsk/track-record/internal/statement"
)

// runFlags are the flags run and key share.
type runFlags struct {
	name, namespace, server, token, salt string
	inputs, tools, env                   list
	platform, fresh, allowUntracked      bool
	timeout                              time.Duration
	argv                                 []string
	// tokenErr is why the token file could not be read.
	tokenErr error
}

func parseRunFlags(cmd string, args []string, stderr io.Writer) (*runFlags, error) {
	fs := newFlagSet(cmd, cmd+" [flags] -- <command> [args...]", stderr)
	f := &runFlags{}
	fs.StringVar(&f.name, "name", "", "what the command is, such as check-model; part of the key (required)")
	fs.StringVar(&f.namespace, "namespace", "", "the store's namespace; part of the key ($TRACK_RECORD_NAMESPACE)")
	fs.StringVar(&f.server, "server", "", "the store's URL ($TRACK_RECORD_URL)")
	fs.StringVar(&f.token, "token", "", "a token for the namespace ($TRACK_RECORD_TOKEN, or the file $TRACK_RECORD_TOKEN_FILE names)")
	fs.Var(&f.inputs, "inputs", "a git `pathspec` of the command's input paths; repeatable (default the whole tracked tree)")
	fs.Var(&f.tools, "tool", "a shell `command` whose standard output and standard error go into the key, such as 'java -version'; repeatable")
	fs.Var(&f.env, "env", "the `name` of a variable whose value goes into the key; repeatable")
	fs.BoolVar(&f.platform, "platform", false, "put this OS and architecture into the key")
	fs.StringVar(&f.salt, "salt", "", "a salt for the key; changing it disowns every earlier record ($TRACK_RECORD_SALT)")
	fs.BoolVar(&f.fresh, "fresh", false, "skip the lookup, run, and still record a pass ($TRACK_RECORD_FRESH)")
	fs.DurationVar(&f.timeout, "timeout", 5*time.Second, "cap on each store request")
	fs.BoolVar(&f.allowUntracked, "allow-untracked", false, "put untracked files into the key instead of skipping the lookup and the record")
	err := parse(fs, args, []envFlag{
		{"namespace", "TRACK_RECORD_NAMESPACE"},
		{"server", "TRACK_RECORD_URL"},
		{"token", "TRACK_RECORD_TOKEN"},
		{"salt", "TRACK_RECORD_SALT"},
		{"fresh", "TRACK_RECORD_FRESH"},
	})
	if err != nil {
		return nil, err
	}
	f.argv = fs.Args()
	switch {
	case f.name == "":
		return nil, usageErrorf(fs, "--name is required")
	case len(f.argv) == 0:
		return nil, usageErrorf(fs, "no command: put it after --")
	case f.namespace != "" && !api.ValidNamespace(f.namespace):
		return nil, usageErrorf(fs, "invalid namespace %q", f.namespace)
	case f.timeout <= 0:
		return nil, usageErrorf(fs, "--timeout must be positive")
	}
	for _, name := range f.env {
		if name == "" || strings.Contains(name, "=") {
			return nil, usageErrorf(fs, "invalid --env %q: give a variable's name", name)
		}
	}
	// A token has no white space, so any around it is a stray newline or
	// the like from wherever it was pasted or mounted.
	f.token = strings.TrimSpace(f.token)
	if path := os.Getenv("TRACK_RECORD_TOKEN_FILE"); f.token == "" && path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			f.tokenErr = fmt.Errorf("cannot read TRACK_RECORD_TOKEN_FILE: %w", err)
		}
		f.token = strings.TrimSpace(string(b))
	}
	return f, nil
}

func (f *runFlags) options() inputs.Options {
	o := inputs.Options{
		Namespace:      f.namespace,
		Name:           f.name,
		Argv:           f.argv,
		Pathspecs:      f.inputs,
		Tools:          f.tools,
		Env:            f.env,
		Salt:           f.salt,
		AllowUntracked: f.allowUntracked,
	}
	if f.platform {
		o.Platform = runtime.GOOS + "/" + runtime.GOARCH
	}
	return o
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	f, err := parseRunFlags("run", args, stderr)
	if err != nil {
		return exitCode(err)
	}
	plain := func(why string, args ...any) int {
		fmt.Fprintf(stderr, "track-record: "+why+"; running %s unrecorded\n", append(args, f.name)...)
		return execute(f.argv, stdin, stdout, stderr).exit(stderr)
	}
	switch {
	case f.tokenErr != nil:
		return plain("%v", f.tokenErr)
	case f.server == "" || f.token == "":
		return plain("no store configured (TRACK_RECORD_URL and TRACK_RECORD_TOKEN)")
	case f.namespace == "":
		fmt.Fprintln(stderr, "track-record run: a store needs --namespace or TRACK_RECORD_NAMESPACE")
		return 2
	}
	cl, err := client.New(f.server, f.token, f.timeout, "track-record/"+Version())
	if err != nil {
		fmt.Fprintf(stderr, "track-record run: %v\n", err)
		return 2
	}
	ctx := context.Background()
	res, err := collect(ctx, f)
	var inputErr *inputs.InputError
	switch {
	case errors.As(err, &inputErr):
		return plain("warning: cannot key the command: %v", err)
	case err != nil:
		fmt.Fprintf(stderr, "track-record: %v\n", err)
		return 2
	case len(res.Untracked) > 0:
		reportUntracked(stderr, f.name, res.Untracked)
		return execute(f.argv, stdin, stdout, stderr).exit(stderr)
	}
	in := res.Inputs
	k := in.Key()
	if !f.fresh && reuse(ctx, cl, f, k, stdout, stderr) {
		return 0
	}

	logFile, err := os.CreateTemp("", "track-record-*.log")
	if err != nil {
		return plain("cannot keep the log: %v", err)
	}
	defer os.Remove(logFile.Name())
	defer logFile.Close()
	capture := &capture{w: logFile, h: sha256.New()}
	intr := catchInterrupts()
	defer intr.stop()
	out := intr.execute(f.argv, stdin, io.MultiWriter(stdout, capture), io.MultiWriter(stderr, capture))
	if out.err != nil || out.code != 0 {
		return out.exit(stderr)
	}
	// A signal sent to the whole group is pending for track-record before
	// the command's exit can be reaped, though no thread may have handled
	// it yet. Starting a process blocks signals and then unblocks them,
	// which delivers a pending one at once; so the recheck, which starts
	// git first, comes before stop, and stop sees the signal. A signal
	// caught during the recheck ends it early: nothing is recorded after
	// one.
	recheck, unwatch := intr.watch(ctx)
	changed := changedSince(recheck, f, k)
	unwatch()
	switch {
	case intr.stop():
		warnf(stderr, "%s exited 0 after an interrupt, so it is not recorded", f.name)
	case out.ioErr != nil:
		warnf(stderr, "%s passed but its output was not passed on in full, so it is not recorded: %v", f.name, out.ioErr)
	case capture.err != nil:
		warnf(stderr, "%s passed but its log could not be kept, so it is not recorded: %v", f.name, capture.err)
	case changed != "":
		warnf(stderr, "%s passed but is not recorded: %s", f.name, changed)
	default:
		record(ctx, cl, f, in, logFile, capture, stderr)
	}
	return 0
}

// changedSince reads the inputs again and says how they no longer give the
// key k, or returns "" if they still do. After an edit or a new file during
// the run, the command may have passed on a tree k does not describe.
func changedSince(ctx context.Context, f *runFlags, k string) string {
	res, err := collect(ctx, f)
	switch {
	case err != nil:
		return fmt.Sprintf("its inputs could not be read again: %v", err)
	case len(res.Untracked) > 0:
		return fmt.Sprintf("untracked files appeared within the inputs while it ran, such as %s", res.Untracked[0])
	case res.Inputs.Key() != k:
		return "its inputs changed while it ran"
	}
	return ""
}

// collect reads the key's inputs from the working directory.
func collect(ctx context.Context, f *runFlags) (inputs.Result, error) {
	wd, err := os.Getwd()
	if err != nil {
		return inputs.Result{}, err
	}
	return inputs.Collect(ctx, wd, f.options(), os.LookupEnv)
}

func reportUntracked(stderr io.Writer, name string, paths []string) {
	const shown = 10
	files := "files"
	if len(paths) == 1 {
		files = "file"
	}
	fmt.Fprintf(stderr, "track-record: %d untracked %s within the inputs, so %s runs with no lookup and no record (add, ignore or remove them, or pass --allow-untracked):\n", len(paths), files, name)
	for i, p := range paths {
		if i == shown {
			fmt.Fprintf(stderr, "track-record:   and %d more\n", len(paths)-shown)
			break
		}
		fmt.Fprintf(stderr, "track-record:   %s\n", p)
	}
}

// reuse looks the key up and, on a valid hit, replays the log and reports
// the pass. Anything but a valid hit is a miss.
func reuse(ctx context.Context, cl *client.Client, f *runFlags, k string, stdout, stderr io.Writer) bool {
	rec, err := cl.Record(ctx, f.namespace, k)
	if errors.Is(err, client.ErrNotFound) {
		return false
	}
	if err != nil {
		warnf(stderr, "lookup failed, so %s runs: %v", f.name, err)
		return false
	}
	if err := check(rec, k, f.name); err != nil {
		warnf(stderr, "ignoring the record for %s, which runs: %v", f.name, err)
		return false
	}
	if data, err := fetchLog(ctx, cl, f.namespace, rec.Log); err != nil {
		fmt.Fprintf(stderr, "track-record: the log is unavailable: %v\n", err)
	} else {
		stdout.Write(data)
	}
	fmt.Fprintf(stderr, "track-record: REUSED %s (recorded %s by %s)\n", f.name, rec.RecordedAt.UTC().Format(time.RFC3339), rec.Writer.Label)
	return true
}

// check reports why a record is not a valid hit for the key and name.
func check(rec api.Record, k, name string) error {
	s, err := statement.Parse(rec.Statement, k)
	if err != nil {
		return err
	}
	if s.Subject[0].Name != name || rec.Name != name {
		return fmt.Errorf("it is for %q, not %q", s.Subject[0].Name, name)
	}
	return nil
}

func fetchLog(ctx context.Context, cl *client.Client, namespace string, ref *api.Log) ([]byte, error) {
	if ref == nil {
		return nil, errors.New("none was stored")
	}
	data, err := cl.Log(ctx, namespace, ref.Digest)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != ref.Size || key.Digest(data) != ref.Digest {
		return nil, errors.New("it does not match its digest")
	}
	return data, nil
}

// record uploads the log, then records the pass. It only warns on failure.
func record(ctx context.Context, cl *client.Client, f *runFlags, in key.Inputs, logFile *os.File, c *capture, stderr io.Writer) {
	var ref *api.Log
	digest := hex.EncodeToString(c.h.Sum(nil))
	_, err := logFile.Seek(0, io.SeekStart)
	if err == nil {
		err = cl.PutLog(ctx, f.namespace, digest, logFile, c.n)
	}
	var se *client.StatusError
	switch {
	case err == nil:
		ref = &api.Log{Digest: digest, Size: c.n}
	case errors.As(err, &se) && se.Code == http.StatusRequestEntityTooLarge:
		warnf(stderr, "the log is too large for the store (%v); recording %s without it", err, f.name)
	default:
		warnf(stderr, "%s passed but is not recorded: storing its log: %v", f.name, err)
		return
	}
	raw, err := json.Marshal(statement.New(in))
	if err != nil {
		warnf(stderr, "%s passed but is not recorded: %v", f.name, err)
		return
	}
	created, err := cl.PutRecord(ctx, f.namespace, in.Key(), api.PutRecord{Statement: raw, Log: ref})
	switch {
	case err != nil:
		warnf(stderr, "%s passed but is not recorded: %v", f.name, err)
	case created:
		fmt.Fprintf(stderr, "track-record: RECORDED %s\n", f.name)
	default:
		fmt.Fprintf(stderr, "track-record: %s was already on record; the first record stands\n", f.name)
	}
}

func warnf(stderr io.Writer, format string, args ...any) {
	fmt.Fprintf(stderr, "track-record: warning: "+format+"\n", args...)
}

// capture keeps a combined copy of a command's output. It never fails a
// write, so a copy that cannot be kept does not stop the output passing
// through; err records why the copy is incomplete.
type capture struct {
	mu  sync.Mutex
	w   io.Writer
	h   hash.Hash
	n   int64
	err error
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		_, c.err = c.w.Write(p)
	}
	c.h.Write(p)
	c.n += int64(len(p))
	return len(p), nil
}

func keyCommand(args []string, stdout, stderr io.Writer) int {
	f, err := parseRunFlags("key", args, stderr)
	if err != nil {
		return exitCode(err)
	}
	if f.namespace == "" {
		fmt.Fprintln(stderr, "track-record key: the key needs --namespace or TRACK_RECORD_NAMESPACE")
		return 2
	}
	res, err := collect(context.Background(), f)
	if err != nil {
		fmt.Fprintf(stderr, "track-record: %v\n", err)
		return 2
	}
	in := res.Inputs.Canonical()
	fmt.Fprintln(stdout, in.Key())
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	kinds := map[key.Kind]int{}
	for _, file := range in.Files {
		kinds[file.Kind]++
	}
	fmt.Fprintf(w, "namespace\t%s\n", in.Namespace)
	fmt.Fprintf(w, "name\t%s\n", in.Name)
	fmt.Fprintf(w, "dir\t%s\n", in.Dir)
	fmt.Fprintf(w, "argv\t%d\n", len(in.Argv))
	fmt.Fprintf(w, "files\t%d: %d %s, %d %s, %d %s, %d %s\n", len(in.Files),
		kinds[key.Regular], key.Regular, kinds[key.Executable], key.Executable,
		kinds[key.Symlink], key.Symlink, kinds[key.Deleted], key.Deleted)
	firstFew(w, pathsOf(in.Files))
	switch {
	case len(res.Untracked) > 0:
		fmt.Fprintf(w, "untracked\t%d, not in the key: run would skip the lookup and the record\n", len(res.Untracked))
		firstFew(w, res.Untracked)
	default:
		fmt.Fprintf(w, "untracked\t%d\n", len(in.Untracked))
		firstFew(w, pathsOf(in.Untracked))
	}
	fmt.Fprintf(w, "tools\t%d\n", len(in.Tools))
	fmt.Fprintf(w, "env\t%d\n", len(in.Env))
	fmt.Fprintf(w, "platform\t%s\n", or(in.Platform, "not in the key"))
	salt := "none"
	if in.Salt != "" {
		salt = "set"
	}
	fmt.Fprintf(w, "salt\t%s\n", salt)
	w.Flush()
	return 0
}

// firstFew lists the first few paths.
func firstFew(w io.Writer, paths []string) {
	const shown = 5
	for i, p := range paths {
		if i == shown {
			fmt.Fprintf(w, "\t  and %d more\n", len(paths)-shown)
			return
		}
		fmt.Fprintf(w, "\t  %s\n", p)
	}
}

func pathsOf(files []key.File) []string {
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	return paths
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
