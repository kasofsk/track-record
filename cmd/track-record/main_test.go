package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kasofsk/track-record/internal/api"
	"github.com/kasofsk/track-record/internal/gittest"
	"github.com/kasofsk/track-record/internal/server"
	"github.com/kasofsk/track-record/internal/statement"
	"github.com/kasofsk/track-record/internal/store"
)

// The test binary doubles as track-record: run with TRACK_RECORD_TEST_MAIN=1
// it is the command, so these tests drive it as a separate process, against
// a store served in this one.
func TestMain(m *testing.M) {
	if os.Getenv("TRACK_RECORD_TEST_MAIN") == "1" {
		main()
		return
	}
	gittest.Hermetic()
	os.Exit(m.Run())
}

// probe is a command that counts its runs in $MARKER and prints a line.
var probe = []string{"sh", "-c", `echo run >> "$MARKER"; echo out; exit ${CODE:-0}`}

type world struct {
	t       *testing.T
	repo    string
	store   *store.Store
	handler http.Handler
	url     string
	tokens  map[string]string // write and read for acme, other for another namespace
	marker  string
	env     []string // added to every run's environment
}

func newWorld(t *testing.T) *world {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	w := &world{
		t:      t,
		repo:   gittest.Repo(t, map[string]string{"README.md": "hello\n", "src/main.go": "package main\n"}),
		store:  st,
		tokens: map[string]string{},
		marker: filepath.Join(t.TempDir(), "runs"),
	}
	w.handler = server.New(st, server.Config{TTL: time.Hour, MaxLog: 1 << 20, MaxStatement: 64 << 10})
	srv := httptest.NewServer(w.handler)
	t.Cleanup(srv.Close)
	w.url = srv.URL
	for name, spec := range map[string][2]string{"write": {"acme", "write"}, "read": {"acme", "read"}, "other": {"other", "write"}} {
		secret, _, err := st.CreateToken(context.Background(), spec[0], store.Scope(spec[1]), "ci-"+name, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		w.tokens[name] = secret
	}
	return w
}

type result struct {
	code           int
	stdout, stderr string
}

// command returns track-record with args, in the repository, with the store
// configured unless env overrides it.
func (w *world) command(args ...string) *exec.Cmd {
	exe, err := os.Executable()
	if err != nil {
		w.t.Fatal(err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = w.repo
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TRACK_RECORD_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env,
		"TRACK_RECORD_TEST_MAIN=1",
		// A race-enabled binary otherwise waits a second before exiting 0.
		"GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0",
		"TRACK_RECORD_URL="+w.url,
		"TRACK_RECORD_TOKEN="+w.tokens["write"],
		"TRACK_RECORD_NAMESPACE=acme",
		"MARKER="+w.marker,
	)
	cmd.Env = append(cmd.Env, w.env...)
	return cmd
}

func (w *world) run(args ...string) result {
	w.t.Helper()
	cmd := w.command(args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		w.t.Fatal(err)
	}
	return result{cmd.ProcessState.ExitCode(), stdout.String(), stderr.String()}
}

// probe runs the probe under track-record with extra flags.
func (w *world) probe(flags ...string) result {
	w.t.Helper()
	args := append([]string{"run", "--name", "probe"}, flags...)
	return w.run(append(append(args, "--"), probe...)...)
}

// key returns the key track-record computes for the probe.
func (w *world) key(flags ...string) string {
	w.t.Helper()
	args := append([]string{"key", "--name", "probe"}, flags...)
	r := w.run(append(append(args, "--"), probe...)...)
	if r.code != 0 {
		w.t.Fatalf("key: %d %s", r.code, r.stderr)
	}
	return strings.SplitN(r.stdout, "\n", 2)[0]
}

// runs is how many times the probe has run.
func (w *world) runs() int {
	b, err := os.ReadFile(w.marker)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		w.t.Fatal(err)
	}
	return strings.Count(string(b), "run\n")
}

func (w *world) expect(r result, code, runs int, stderr ...string) {
	w.t.Helper()
	if r.code != code {
		w.t.Errorf("exit %d, want %d\nstderr: %s", r.code, code, r.stderr)
	}
	if n := w.runs(); n != runs {
		w.t.Errorf("the probe has run %d times, want %d\nstderr: %s", n, runs, r.stderr)
	}
	for _, s := range stderr {
		if !strings.Contains(r.stderr, s) {
			w.t.Errorf("stderr does not contain %q:\n%s", s, r.stderr)
		}
	}
}

var reused = regexp.MustCompile(`(?m)^track-record: REUSED probe \(recorded \d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ by ci-write\)$`)

func TestHitAndMiss(t *testing.T) {
	w := newWorld(t)
	r := w.probe()
	w.expect(r, 0, 1, "RECORDED probe")
	if r.stdout != "out\n" {
		t.Errorf("stdout = %q", r.stdout)
	}

	r = w.probe()
	w.expect(r, 0, 1)
	if !reused.MatchString(r.stderr) || r.stdout != "out\n" {
		t.Errorf("a hit printed stdout %q, stderr %q", r.stdout, r.stderr)
	}

	gittest.Write(t, w.repo, "README.md", "edited\n")
	w.expect(w.probe(), 0, 2, "RECORDED probe")
	w.expect(w.probe(), 0, 2, "REUSED probe")
	gittest.Commit(t, w.repo)
	w.expect(w.probe(), 0, 2, "REUSED probe")
}

func TestLogReplayedOnHit(t *testing.T) {
	w := newWorld(t)
	script := []string{"run", "--name", "probe", "--", "sh", "-c", `echo run >> "$MARKER"; echo one; echo two >&2; echo three`}
	first := w.run(script...)
	w.expect(first, 0, 1, "two")
	hit := w.run(script...)
	w.expect(hit, 0, 1, "REUSED probe")
	lines := strings.Split(strings.TrimSpace(hit.stdout), "\n")
	slices.Sort(lines)
	if !slices.Equal(lines, []string{"one", "three", "two"}) {
		t.Errorf("replayed %q, want the combined output", hit.stdout)
	}
	if !strings.Contains(hit.stdout, "one\n") || strings.Index(hit.stdout, "one") > strings.Index(hit.stdout, "three") {
		t.Errorf("stdout lines out of order in %q", hit.stdout)
	}
}

func TestLogCapturedUnderTMPDIR(t *testing.T) {
	w := newWorld(t)
	tmp := t.TempDir()
	w.env = append(w.env, "TMPDIR="+tmp)
	r := w.run("run", "--name", "ls", "--", "sh", "-c", `ls "$TMPDIR"`)
	if r.code != 0 || !regexp.MustCompile(`(?m)^track-record-.*\.log$`).MatchString(r.stdout) {
		t.Errorf("the log was not captured under TMPDIR: %q %q", r.stdout, r.stderr)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("left behind in TMPDIR: %v", left)
	}
}

func TestFailuresAreNotRecorded(t *testing.T) {
	for _, code := range []int{1, 2, 3} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			w := newWorld(t)
			w.env = append(w.env, "CODE="+strconv.Itoa(code))
			r := w.probe()
			w.expect(r, code, 1)
			if strings.Contains(r.stderr, "RECORDED") {
				t.Errorf("recorded: %s", r.stderr)
			}
			w.expect(w.probe(), code, 2)
			if _, err := w.store.Record(context.Background(), "acme", w.key(), time.Now()); err != store.ErrNotFound {
				t.Errorf("a record exists: %v", err)
			}
		})
	}
}

func TestOwnFailuresExit2(t *testing.T) {
	tests := []struct {
		name  string
		setup func(w *world) []string
	}{
		{"no name", func(w *world) []string { return append([]string{"run", "--"}, probe...) }},
		{"no command", func(w *world) []string { return []string{"run", "--name", "probe", "--"} }},
		{"unknown flag", func(w *world) []string {
			return append([]string{"run", "--name", "probe", "--bogus", "--"}, probe...)
		}},
		{"bad namespace", func(w *world) []string {
			return append([]string{"run", "--name", "probe", "--namespace", "Not/Valid", "--"}, probe...)
		}},
		{"no namespace", func(w *world) []string {
			w.env = append(w.env, "TRACK_RECORD_NAMESPACE=")
			return append([]string{"run", "--name", "probe", "--"}, probe...)
		}},
		{"bad server", func(w *world) []string {
			w.env = append(w.env, "TRACK_RECORD_URL=localhost:8080")
			return append([]string{"run", "--name", "probe", "--"}, probe...)
		}},
		{"not a git checkout", func(w *world) []string {
			w.repo = w.t.TempDir()
			return append([]string{"run", "--name", "probe", "--"}, probe...)
		}},
		{"pathspec matches nothing", func(w *world) []string {
			return append([]string{"run", "--name", "probe", "--inputs", "srcc", "--"}, probe...)
		}},
		{"command not found", func(w *world) []string {
			return []string{"run", "--name", "probe", "--", "no-such-command-anywhere"}
		}},
		{"bad TRACK_RECORD_FRESH", func(w *world) []string {
			w.env = append(w.env, "TRACK_RECORD_FRESH=maybe")
			return append([]string{"run", "--name", "probe", "--"}, probe...)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.expect(w.run(tt.setup(w)...), 2, 0)
		})
	}
}

func TestNoStore(t *testing.T) {
	w := newWorld(t)
	w.env = append(w.env, "TRACK_RECORD_URL=", "CODE=3")
	r := w.probe()
	w.expect(r, 3, 1)
	if r.stderr != "track-record: no store configured (TRACK_RECORD_URL and TRACK_RECORD_TOKEN); running probe unrecorded\n" {
		t.Errorf("stderr = %q, want one line", r.stderr)
	}
	// Plainly means plainly: not even a git checkout is needed.
	w.repo = t.TempDir()
	w.expect(w.probe(), 3, 2)
}

func TestUnreachableStore(t *testing.T) {
	w := newWorld(t)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	w.url = gone.URL
	w.expect(w.probe(), 0, 1, "warning: lookup failed", "not recorded")
	w.expect(w.probe(), 0, 2, "warning: lookup failed")
}

func TestStoreTimeout(t *testing.T) {
	w := newWorld(t)
	hang := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// Until the body is read, the server cannot see the client go.
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(hang.Close)
	w.url = hang.URL
	start := time.Now()
	w.expect(w.probe("--timeout", "200ms"), 0, 1, "warning: lookup failed", "not recorded")
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	}
}

// stub serves a fixed answer to record lookups and accepts every put.
func (w *world) stub(record func(r *http.Request) (int, []byte), log func() (int, []byte)) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/records/"):
			code, body := record(r)
			rw.WriteHeader(code)
			rw.Write(body)
		case r.Method == "GET" && log != nil:
			code, body := log()
			rw.WriteHeader(code)
			rw.Write(body)
		case r.Method == "PUT":
			io.Copy(io.Discard, r.Body)
			rw.WriteHeader(http.StatusCreated)
			rw.Write([]byte(`{}`))
		default:
			http.NotFound(rw, r)
		}
	}))
	w.t.Cleanup(srv.Close)
	w.url = srv.URL
}

// recordFor returns a record of the probe passing under k, changed by change.
func recordFor(t *testing.T, k string, change func(s *statement.Statement, r *api.Record)) []byte {
	t.Helper()
	s := statement.Statement{
		Type:          statement.Type,
		Subject:       []statement.ResourceDescriptor{{Name: "probe", Digest: map[string]string{"sha256": k}}},
		PredicateType: statement.PredicateType,
		Predicate: statement.TestResult{
			Result:        statement.Passed,
			Configuration: []statement.ResourceDescriptor{{Name: "argv", Digest: map[string]string{"sha256": k}}},
		},
	}
	r := api.Record{Name: "probe", Writer: api.Writer{ID: 1, Label: "someone"}, RecordedAt: time.Now().UTC()}
	change(&s, &r)
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	r.Statement = raw
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMalformedRecordIsAMiss(t *testing.T) {
	other := strings.Repeat("0", 64)
	tests := []struct {
		name string
		body func(t *testing.T, k string) []byte
	}{
		{"not json", func(t *testing.T, k string) []byte { return []byte("{") }},
		{"empty", func(t *testing.T, k string) []byte { return []byte("{}") }},
		{"another key", func(t *testing.T, k string) []byte {
			return recordFor(t, k, func(s *statement.Statement, r *api.Record) { s.Subject[0].Digest["sha256"] = other })
		}},
		{"another name", func(t *testing.T, k string) []byte {
			return recordFor(t, k, func(s *statement.Statement, r *api.Record) { s.Subject[0].Name = "lint" })
		}},
		{"record names another", func(t *testing.T, k string) []byte {
			return recordFor(t, k, func(s *statement.Statement, r *api.Record) { r.Name = "lint" })
		}},
		{"failed", func(t *testing.T, k string) []byte {
			return recordFor(t, k, func(s *statement.Statement, r *api.Record) { s.Predicate.Result = "FAILED" })
		}},
		{"not a test result", func(t *testing.T, k string) []byte {
			return recordFor(t, k, func(s *statement.Statement, r *api.Record) { s.PredicateType = "https://slsa.dev/provenance/v1" })
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			k := w.key()
			w.stub(func(*http.Request) (int, []byte) { return http.StatusOK, tt.body(t, k) }, nil)
			r := w.probe()
			w.expect(r, 0, 1, "warning:")
			if strings.Contains(r.stderr, "REUSED") {
				t.Errorf("reused a malformed record: %s", r.stderr)
			}
		})
	}
}

func TestHitWithoutItsLog(t *testing.T) {
	tests := []struct {
		name string
		log  func() (int, []byte)
	}{
		{"missing", func() (int, []byte) { return http.StatusNotFound, []byte(`{"error":"no log"}`) }},
		{"not matching its digest", func() (int, []byte) { return http.StatusOK, []byte("fake") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			k := w.key()
			w.stub(func(*http.Request) (int, []byte) {
				return http.StatusOK, recordFor(t, k, func(s *statement.Statement, r *api.Record) {
					r.Log = &api.Log{Digest: strings.Repeat("a", 64), Size: 4}
				})
			}, tt.log)
			r := w.probe()
			w.expect(r, 0, 0, "the log is unavailable", "REUSED probe")
			if r.stdout != "" {
				t.Errorf("stdout = %q", r.stdout)
			}
		})
	}
}

func TestLogTooLargeIsLeftOut(t *testing.T) {
	w := newWorld(t)
	args := []string{"run", "--name", "big", "--", "sh", "-c", `echo run >> "$MARKER"; head -c 1100000 /dev/zero`}
	r := w.run(args...)
	w.expect(r, 0, 1, "the log is too large", "RECORDED big")
	if len(r.stdout) != 1100000 {
		t.Errorf("passed through %d bytes", len(r.stdout))
	}
	r = w.run(args...)
	w.expect(r, 0, 1, "the log is unavailable", "REUSED big")
	if r.stdout != "" {
		t.Errorf("replayed %d bytes", len(r.stdout))
	}
}

func TestUntrackedGuard(t *testing.T) {
	w := newWorld(t)
	w.expect(w.probe(), 0, 1, "RECORDED")

	// With an untracked file there is no lookup, though a record exists for
	// the tracked tree, and no record.
	gittest.Write(t, w.repo, "src/new.go", "package main\n")
	r := w.probe()
	w.expect(r, 0, 2, "1 untracked file within", "track-record:   src/new.go\n")
	if strings.Contains(r.stderr, "REUSED") || strings.Contains(r.stderr, "RECORDED") {
		t.Errorf("looked up or recorded: %s", r.stderr)
	}

	// Many are counted and the first ten named.
	for i := range 11 {
		gittest.Write(t, w.repo, "more/"+strconv.Itoa(i+10), "x")
	}
	r = w.probe()
	w.expect(r, 0, 3, "12 untracked files", "and 2 more")
	if n := strings.Count(r.stderr, "track-record:   more/"); n != 10 {
		t.Errorf("%d paths named under more/, want 10:\n%s", n, r.stderr)
	}

	// Allowed in, they are part of the key.
	w.expect(w.probe("--allow-untracked"), 0, 4, "RECORDED")
	w.expect(w.probe("--allow-untracked"), 0, 4, "REUSED")
	gittest.Write(t, w.repo, "src/new.go", "package main // edited\n")
	w.expect(w.probe("--allow-untracked"), 0, 5, "RECORDED")

	// Ignored files never count.
	if err := os.RemoveAll(filepath.Join(w.repo, "more")); err != nil {
		t.Fatal(err)
	}
	gittest.Write(t, w.repo, ".gitignore", "*.log\n")
	gittest.Git(t, w.repo, "add", ".gitignore", "src/new.go")
	w.expect(w.probe(), 0, 6, "RECORDED")
	gittest.Write(t, w.repo, "build.log", "x")
	w.expect(w.probe(), 0, 6, "REUSED")
}

func TestInputsChangedDuringTheRun(t *testing.T) {
	tests := []struct {
		name     string
		flags    []string
		edit     string // what happens to the tree while the command runs
		recorded bool
		stderr   string
	}{
		{"tracked file edited", nil, `echo good > value.txt`, false, "check passed but is not recorded: its inputs changed while it ran"},
		{"tracked file deleted", nil, `rm value.txt`, false, "its inputs changed while it ran"},
		{"untracked file added", nil, `touch new.txt`, false, "untracked files appeared within the inputs while it ran, such as new.txt"},
		{"untracked file allowed", []string{"--allow-untracked"}, `touch new.txt`, false, "its inputs changed while it ran"},
		{"file outside the inputs", []string{"--inputs", "src"}, `echo good > value.txt`, true, "RECORDED check"},
		{"ignored file written", nil, `mkdir -p out && echo x > out/a`, true, "RECORDED check"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			gittest.Write(t, w.repo, "value.txt", "bad\n")
			gittest.Write(t, w.repo, ".gitignore", "out/\n")
			gittest.Commit(t, w.repo)
			args := append(append([]string{"run", "--name", "check"}, tt.flags...), "--", "sh", "-c", tt.edit)
			r := w.run(args...)
			if r.code != 0 || !strings.Contains(r.stderr, tt.stderr) {
				t.Fatalf("exit %d, stderr %q; want 0 and %q", r.code, r.stderr, tt.stderr)
			}
			if got := strings.Contains(r.stderr, "RECORDED"); got != tt.recorded {
				t.Errorf("recorded = %v, want %v: %s", got, tt.recorded, r.stderr)
			}
			if lines := strings.Count(r.stderr, "\n"); lines != 1 {
				t.Errorf("%d lines on stderr, want 1: %s", lines, r.stderr)
			}
		})
	}
}

// An edit saved while a check runs, then undone, must not leave the
// undone tree passing on the strength of the edit.
func TestEditDuringTheRunThenUndone(t *testing.T) {
	w := newWorld(t)
	gittest.Write(t, w.repo, "value.txt", "bad\n")
	gittest.Commit(t, w.repo)
	gate := filepath.Join(t.TempDir(), "go")
	w.env = append(w.env, "GATE="+gate)
	script := `echo ready; while [ ! -f "$GATE" ]; do sleep 0.01; done; grep -qx good value.txt`
	cmd := w.command("run", "--name", "check", "--", "sh", "-c", script)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr syncBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("read %q, %v; stderr: %s", line, err, stderr.String())
	}
	gittest.Write(t, w.repo, "value.txt", "good\n")
	gittest.Write(t, filepath.Dir(gate), "go", "")
	cmd.Wait()
	if code := cmd.ProcessState.ExitCode(); code != 0 || strings.Contains(stderr.String(), "RECORDED") {
		t.Fatalf("exit %d; stderr: %s", code, stderr.String())
	}
	gittest.Git(t, w.repo, "checkout", "--", "value.txt")
	if r := w.run("run", "--name", "check", "--", "sh", "-c", script); r.code != 1 {
		t.Errorf("the undone tree: exit %d, want 1; stderr: %s", r.code, r.stderr)
	}
}

func TestFresh(t *testing.T) {
	w := newWorld(t)
	w.expect(w.probe(), 0, 1, "RECORDED")
	w.expect(w.probe("--fresh"), 0, 2, "already on record")
	w.env = append(w.env, "TRACK_RECORD_FRESH=1")
	w.expect(w.probe(), 0, 3, "already on record")

	// Fresh still records a pass where there is none.
	gittest.Write(t, w.repo, "README.md", "edited\n")
	w.expect(w.probe(), 0, 4, "RECORDED")
}

func TestSignalPassthrough(t *testing.T) {
	const (
		trapped = `trap 'exit 7' INT TERM; echo ready; while :; do sleep 0.05; done`
		passing = `trap 'exit 0' INT TERM; echo ready; while :; do sleep 0.05; done`
		killed  = `echo ready; exec sleep 30`
	)
	tests := []struct {
		name   string
		signal syscall.Signal
		group  bool // to the process group, as a terminal or a control-group stop sends it
		script string
		want   int
	}{
		{"handled", syscall.SIGTERM, false, trapped, 7},
		{"killed", syscall.SIGTERM, false, killed, 128 + int(syscall.SIGTERM)},
		{"interrupted", syscall.SIGINT, false, killed, 128 + int(syscall.SIGINT)},
		{"handled and passing", syscall.SIGTERM, false, passing, 0},
		{"group handled", syscall.SIGTERM, true, trapped, 7},
		{"group killed", syscall.SIGTERM, true, killed, 128 + int(syscall.SIGTERM)},
		{"group interrupted", syscall.SIGINT, true, killed, 128 + int(syscall.SIGINT)},
		{"group handled and passing", syscall.SIGINT, true, passing, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			code, stderr := w.signalled(tt.signal, tt.group, tt.script)
			if code != tt.want {
				t.Errorf("exit %d, want %d; stderr: %s", code, tt.want, stderr)
			}
			if strings.Contains(stderr, "RECORDED") {
				t.Errorf("an interrupted run was recorded: %s", stderr)
			}
		})
	}
}

// A signal sent to the group reaches the command and track-record at once,
// so the command can exit 0 before track-record has handled its copy.
func TestGroupInterruptIsNeverRecorded(t *testing.T) {
	w := newWorld(t)
	const script = `trap 'exit 0' INT; echo ready; while :; do :; done`
	k := w.run("key", "--name", "sig", "--", "sh", "-c", script)
	if k.code != 0 {
		t.Fatalf("key: %d %s", k.code, k.stderr)
	}
	for i := range 100 {
		if code, stderr := w.signalled(syscall.SIGINT, true, script); code != 0 || strings.Contains(stderr, "RECORDED") {
			t.Fatalf("run %d: exit %d; stderr: %s", i, code, stderr)
		}
	}
	key := strings.SplitN(k.stdout, "\n", 2)[0]
	if _, err := w.store.Record(context.Background(), "acme", key, time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an interrupted run is on record: %v", err)
	}
}

// signalled runs script under track-record in a process group of its own,
// sends sig once the script prints "ready", and returns the exit code and
// standard error.
func (w *world) signalled(sig syscall.Signal, group bool, script string) (int, string) {
	w.t.Helper()
	if signal.Ignored(sig) {
		w.t.Skipf("%v is ignored here, and so would be in the command", sig)
	}
	cmd := w.command("run", "--name", "sig", "--", "sh", "-c", script)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		w.t.Fatal(err)
	}
	var stderr syncBuffer
	cmd.Stderr = &stderr
	// In a group of its own, so a signal that is not passed on can fail the
	// test by killing track-record and its command together.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		w.t.Fatal(err)
	}
	stuck := time.AfterFunc(10*time.Second, func() {
		w.t.Errorf("still running 10s after %v; stderr: %s", sig, stderr.String())
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	})
	defer stuck.Stop()
	lines := bufio.NewReader(stdout)
	if line, err := lines.ReadString('\n'); err != nil || line != "ready\n" {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		cmd.Wait()
		w.t.Fatalf("read %q, %v; stderr: %s", line, err, stderr.String())
	}
	pid := cmd.Process.Pid
	if group {
		pid = -pid
	}
	if err := syscall.Kill(pid, sig); err != nil {
		w.t.Fatal(err)
	}
	io.Copy(io.Discard, lines)
	cmd.Wait()
	return cmd.ProcessState.ExitCode(), stderr.String()
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestTokenFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("read with its newline trimmed", func(t *testing.T) {
		w := newWorld(t)
		w.env = append(w.env, "TRACK_RECORD_TOKEN=", "TRACK_RECORD_TOKEN_FILE="+write("good", w.tokens["write"]+"\n", 0o600))
		w.expect(w.probe(), 0, 1, "RECORDED")
		w.expect(w.probe(), 0, 1, "REUSED")
	})
	t.Run("read with any white space trimmed", func(t *testing.T) {
		w := newWorld(t)
		w.env = append(w.env, "TRACK_RECORD_TOKEN=", "TRACK_RECORD_TOKEN_FILE="+write("spaced", " "+w.tokens["write"]+" \r\n\n", 0o600))
		w.expect(w.probe(), 0, 1, "RECORDED")
	})
	t.Run("the variable with white space trimmed", func(t *testing.T) {
		w := newWorld(t)
		w.env = append(w.env, "TRACK_RECORD_TOKEN="+w.tokens["write"]+"\n")
		w.expect(w.probe(), 0, 1, "RECORDED")
	})
	t.Run("the variable wins", func(t *testing.T) {
		w := newWorld(t)
		w.env = append(w.env, "TRACK_RECORD_TOKEN_FILE="+write("wrong", "trk_wrong\n", 0o600))
		w.expect(w.probe(), 0, 1, "RECORDED")
	})
	t.Run("unreadable is no token", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads any file")
		}
		w := newWorld(t)
		path := write("locked", "trk_do_not_print_me\n", 0o000)
		w.env = append(w.env, "TRACK_RECORD_TOKEN=", "TRACK_RECORD_TOKEN_FILE="+path)
		r := w.probe()
		w.expect(r, 0, 1, path)
		if strings.Count(r.stderr, "\n") != 1 || strings.Contains(r.stderr, "do_not_print") || strings.Contains(r.stderr, "RECORDED") {
			t.Errorf("stderr = %q", r.stderr)
		}
	})
	t.Run("missing is no token", func(t *testing.T) {
		w := newWorld(t)
		path := filepath.Join(dir, "missing")
		w.env = append(w.env, "TRACK_RECORD_TOKEN=", "TRACK_RECORD_TOKEN_FILE="+path)
		r := w.probe()
		w.expect(r, 0, 1, path, "unrecorded")
		if strings.Count(r.stderr, "\n") != 1 {
			t.Errorf("stderr = %q, want one line", r.stderr)
		}
	})
}

func TestAuthRefusals(t *testing.T) {
	warnings := regexp.MustCompile(`(?m)^track-record: warning: `)
	tests := []struct {
		name     string
		token    func(w *world) string
		warnings int
		stderr   []string
	}{
		{"another namespace's token", func(w *world) string { return w.tokens["other"] }, 2, []string{"403", "lookup failed", "not recorded"}},
		{"a read token", func(w *world) string { return w.tokens["read"] }, 1, []string{"403", "not recorded"}},
		{"an unknown token", func(w *world) string { return "trk_unknown" }, 2, []string{"401", "lookup failed", "not recorded"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.env = append(w.env, "TRACK_RECORD_TOKEN="+tt.token(w))
			r := w.probe()
			w.expect(r, 0, 1, tt.stderr...)
			if n := len(warnings.FindAllString(r.stderr, -1)); n != tt.warnings {
				t.Errorf("%d warnings, want %d:\n%s", n, tt.warnings, r.stderr)
			}
			w.env = append(w.env, "CODE=1")
			w.expect(w.probe(), 1, 2)
		})
	}
}

func TestRevokedWritersRecordIsAMiss(t *testing.T) {
	w := newWorld(t)
	w.expect(w.probe(), 0, 1, "RECORDED probe")
	tokens, err := w.store.Tokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range tokens {
		if tok.Label == "ci-write" {
			if err := w.store.RevokeToken(context.Background(), tok.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	w.env = append(w.env, "TRACK_RECORD_TOKEN="+w.tokens["read"])
	r := w.probe()
	w.expect(r, 0, 2)
	if strings.Contains(r.stderr, "REUSED") {
		t.Errorf("a revoked writer's record was reused: %s", r.stderr)
	}
}

func TestEnvValuesStayOutOfTheStore(t *testing.T) {
	w := newWorld(t)
	const secret = "s3cr3t-hunter2"
	var mu sync.Mutex
	var seen []string
	inner := w.handler
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.String()+"\n"+string(b))
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(b))
		inner.ServeHTTP(rw, r)
	}))
	t.Cleanup(srv.Close)
	w.url = srv.URL
	w.env = append(w.env, "DATABASE_URL=postgres://app:"+secret+"@db/app")

	w.expect(w.probe("--env", "DATABASE_URL", "--env", "UNSET_VAR"), 0, 1, "RECORDED")
	w.expect(w.probe("--env", "DATABASE_URL", "--env", "UNSET_VAR"), 0, 1, "REUSED")
	mu.Lock()
	defer mu.Unlock()
	var puts int
	for _, req := range seen {
		if strings.HasPrefix(req, "PUT /v1/records/") {
			puts++
			if !strings.Contains(req, `"DATABASE_URL"`) || !strings.Contains(req, `"unset":true`) {
				t.Errorf("the statement does not name the variables: %s", req)
			}
		}
		if strings.Contains(req, secret) {
			t.Errorf("a request carries the value: %s", req)
		}
	}
	if puts != 1 {
		t.Errorf("%d record puts, want 1", puts)
	}
}

func TestTokenStaysOutOfTheCommand(t *testing.T) {
	w := newWorld(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(w.tokens["write"]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string][]string{
		"from TRACK_RECORD_TOKEN":      nil,
		"from TRACK_RECORD_TOKEN_FILE": {"TRACK_RECORD_TOKEN=", "TRACK_RECORD_TOKEN_FILE=" + tokenFile},
	} {
		t.Run(name, func(t *testing.T) {
			w.env = append(slices.Clone(env), "EXE="+exe)
			script := `env; "$EXE" run --name nested -- true`
			r := w.run("run", "--name", "env-"+strings.ReplaceAll(name, " ", "-"), "--", "sh", "-c", script)
			w.env = nil
			if r.code != 0 || !strings.Contains(r.stderr, "RECORDED") {
				t.Fatalf("exit %d; stderr: %s", r.code, r.stderr)
			}
			for _, v := range []string{"TRACK_RECORD_TOKEN=", "TRACK_RECORD_TOKEN_FILE=", w.tokens["write"]} {
				if strings.Contains(r.stdout, v) {
					t.Errorf("the command saw %q:\n%s", v, r.stdout)
				}
			}
			if !strings.Contains(r.stdout, "TRACK_RECORD_URL=") {
				t.Errorf("the command lost the rest of its environment:\n%s", r.stdout)
			}
			if !strings.Contains(r.stderr, "no store configured") {
				t.Errorf("track-record inside the command did not run plainly: %s", r.stderr)
			}
		})
	}
}

func TestKeyCommand(t *testing.T) {
	w := newWorld(t)
	r := w.run("key", "--name", "probe", "--platform", "--salt", "x", "--env", "HOME", "--tool", "true", "--", "true")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	lines := strings.Split(r.stdout, "\n")
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(lines[0]) {
		t.Errorf("first line %q is not a key", lines[0])
	}
	for _, want := range []string{"files      2: 2 file, 0 exec, 0 symlink, 0 deleted", "  README.md", "tools      1", "env        1", "salt       set"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("summary lacks %q:\n%s", want, r.stdout)
		}
	}

	// The key printed is the key run records under.
	w.expect(w.probe(), 0, 1, "RECORDED")
	if _, err := w.store.Record(context.Background(), "acme", w.key(), time.Now()); err != nil {
		t.Errorf("no record under the printed key: %v", err)
	}

	if r := w.run("key", "--name", "probe", "--namespace", "", "--", "true"); r.code != 2 {
		t.Errorf("a key without a namespace: exit %d", r.code)
	}
}

func TestVersion(t *testing.T) {
	w := newWorld(t)
	r := w.run("--version")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "track-record ") {
		t.Errorf("--version: %d %q", r.code, r.stdout)
	}
}

func TestTokenCommandsNeedAnExistingDatabase(t *testing.T) {
	w := newWorld(t)
	dir := t.TempDir()
	notDB := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notDB, []byte("not a database\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, path, want string }{
		{"missing", filepath.Join(dir, "typo.db"), "no database at " + filepath.Join(dir, "typo.db")},
		{"not SQLite", notDB, notDB + " is not a track-record database"},
		{"empty", empty, empty + " is not a track-record database"},
	} {
		for _, args := range [][]string{
			{"token", "create", "--db", c.path, "--namespace", "acme", "--scope", "write", "--label", "ci"},
			{"token", "list", "--db", c.path},
			{"token", "revoke", "--db", c.path, "1"},
		} {
			r := w.run(args...)
			if r.code != 2 || r.stdout != "" || !strings.Contains(r.stderr, c.want) || strings.Count(r.stderr, "\n") != 1 {
				t.Errorf("%s: %q: exit %d, stdout %q, stderr %q", c.name, args[1], r.code, r.stdout, r.stderr)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "typo.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a database was created at the mistyped path: %v", err)
	}
	if b, _ := os.ReadFile(notDB); string(b) != "not a database\n" {
		t.Errorf("the file that is not a database changed: %q", b)
	}
	if info, err := os.Stat(empty); err != nil || info.Size() != 0 {
		t.Errorf("the empty file changed: %v", err)
	}
}

func TestTokenCreateStdoutIsTheToken(t *testing.T) {
	w := newWorld(t)
	db := filepath.Join(t.TempDir(), "admin.db")
	created, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	created.Close()
	r := w.run("token", "create", "--db", db, "--namespace", "acme", "--scope", "write", "--label", "ci")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if !regexp.MustCompile(`\Atrk_[A-Za-z0-9_-]{43}\n\z`).MatchString(r.stdout) {
		t.Errorf("stdout = %q, want the token and a newline", r.stdout)
	}
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tok, err := st.Authenticate(context.Background(), strings.TrimSuffix(r.stdout, "\n"))
	if err != nil || tok.Label != "ci" || tok.Namespace != "acme" || tok.Scope != store.Write {
		t.Errorf("the printed token: %+v, %v", tok, err)
	}

	list := w.run("token", "list", "--db", db)
	if list.code != 0 || !strings.Contains(list.stdout, "acme") || strings.Contains(list.stdout, "trk_") {
		t.Errorf("token list: %d %q", list.code, list.stdout)
	}
	if r := w.run("token", "revoke", "--db", db, strconv.FormatInt(tok.ID, 10)); r.code != 0 {
		t.Errorf("revoke: %d %s", r.code, r.stderr)
	}
	if _, err := st.Authenticate(context.Background(), strings.TrimSuffix(r.stdout, "\n")); err != store.ErrNotFound {
		t.Errorf("revoked token authenticates: %v", err)
	}
	for _, args := range [][]string{
		{"token", "create", "--db", db, "--namespace", "acme", "--scope", "admin", "--label", "x"},
		{"token", "create", "--db", db, "--namespace", "acme", "--scope", "read"},
		{"token", "revoke", "--db", db, "x"},
	} {
		if r := w.run(args...); r.code != 2 || r.stdout != "" {
			t.Errorf("%q: exit %d, stdout %q", args, r.code, r.stdout)
		}
	}
	if r := w.run("token", "revoke", "--db", db, "999"); r.code != 1 {
		t.Errorf("revoking an unknown token: exit %d", r.code)
	}
}
