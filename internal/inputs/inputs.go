// Package inputs reads a command's key inputs: paths from a git checkout as
// they are on disk, tool output and environment variables.
package inputs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/kasofsk/track-record/internal/key"
)

// Options says what goes into a key besides the tracked tree.
type Options struct {
	Namespace      string
	Name           string
	Argv           []string
	Pathspecs      []string // which paths are inputs; none means the whole tree
	Tools          []string
	Env            []string
	Platform       string
	Salt           string
	AllowUntracked bool
}

// Result is what Collect found.
type Result struct {
	Inputs key.Inputs
	// Untracked lists the untracked paths within the pathspecs when they are
	// not allowed into the key. While it is non-empty the key does not
	// describe what a command would check.
	Untracked []string
}

// An InputError is an input that could not be read, such as a file or a tool
// that failed. The command can still run, but not under a key.
type InputError struct{ Err error }

func (e *InputError) Error() string { return e.Err.Error() }
func (e *InputError) Unwrap() error { return e.Err }

// Collect reads the key inputs for a command run in dir. A git failure, such
// as dir not being in a checkout or a pathspec matching nothing, is returned
// as is; an unreadable input is an *InputError.
func Collect(ctx context.Context, dir string, o Options, lookup func(string) (string, bool)) (Result, error) {
	repo, err := Open(ctx, dir)
	if err != nil {
		return Result{}, err
	}
	tracked, err := repo.Tracked(ctx, o.Pathspecs)
	if err != nil {
		return Result{}, err
	}
	untracked, err := repo.Untracked(ctx, o.Pathspecs)
	if err != nil {
		return Result{}, err
	}
	in := key.Inputs{
		Namespace: o.Namespace,
		Name:      o.Name,
		Dir:       repo.Dir,
		Argv:      o.Argv,
		Env:       Env(o.Env, lookup),
		Platform:  o.Platform,
		Salt:      o.Salt,
	}
	var res Result
	if in.Files, err = repo.Hash(tracked); err != nil {
		return Result{}, &InputError{err}
	}
	if o.AllowUntracked {
		if in.Untracked, err = repo.Hash(untracked); err != nil {
			return Result{}, &InputError{err}
		}
	} else {
		res.Untracked = untracked
	}
	if in.Tools, err = Tools(ctx, dir, o.Tools); err != nil {
		return Result{}, &InputError{err}
	}
	res.Inputs = in
	return res, nil
}

// Repo is the git checkout a command runs in.
type Repo struct {
	Root string // the working tree's top level
	Dir  string // the working directory relative to Root, "." at the top
	wd   string // the working directory, which pathspecs are relative to
}

// Open finds the checkout containing dir.
func Open(ctx context.Context, dir string) (*Repo, error) {
	out, err := git(ctx, dir, "rev-parse", "--show-toplevel", "--show-prefix")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 {
		return nil, fmt.Errorf("git rev-parse: unexpected output %q", out)
	}
	rel := strings.TrimSuffix(lines[1], "/")
	if rel == "" {
		rel = "."
	}
	return &Repo{Root: lines[0], Dir: rel, wd: dir}, nil
}

// Tracked lists the tracked paths the pathspecs match, relative to the root
// and in git ls-files order. No pathspecs means the whole tree; a pathspec
// that matches no tracked path is an error.
func (r *Repo) Tracked(ctx context.Context, pathspecs []string) ([]string, error) {
	args := []string{"ls-files", "-z", "--full-name"}
	if len(pathspecs) > 0 {
		args = append(args, "--error-unmatch")
	}
	return r.list(ctx, args, pathspecs)
}

// Untracked lists the untracked paths the pathspecs match that git does not
// ignore.
func (r *Repo) Untracked(ctx context.Context, pathspecs []string) ([]string, error) {
	return r.list(ctx, []string{"ls-files", "-z", "--full-name", "--others", "--exclude-standard"}, pathspecs)
}

func (r *Repo) list(ctx context.Context, args, pathspecs []string) ([]string, error) {
	if len(pathspecs) == 0 {
		pathspecs = []string{":/"}
	}
	args = append(append(args, "--"), pathspecs...)
	out, err := git(ctx, r.wd, args...)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	paths := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	// A path with merge conflicts is listed once per stage.
	return slices.Compact(paths), nil
}

// Hash describes each path as it is on disk.
func (r *Repo) Hash(paths []string) ([]key.File, error) {
	files := make([]key.File, 0, len(paths))
	for _, p := range paths {
		f, err := describe(filepath.Join(r.Root, filepath.FromSlash(p)))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		f.Path = p
		files = append(files, f)
	}
	return files, nil
}

func describe(path string) (key.File, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return key.File{Kind: key.Deleted}, nil
	}
	if err != nil {
		return key.File{}, err
	}
	switch mode := info.Mode(); {
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return key.File{}, err
		}
		return key.File{Kind: key.Symlink, SHA256: key.Digest([]byte(target))}, nil
	case mode.IsRegular():
		digest, err := hashContent(path)
		if err != nil {
			return key.File{}, err
		}
		kind := key.Regular
		if mode&0o100 != 0 {
			kind = key.Executable
		}
		return key.File{Kind: kind, SHA256: digest}, nil
	case mode.IsDir():
		return key.File{}, errors.New("is a directory, such as a submodule; leave it out of --inputs")
	default:
		return key.File{}, fmt.Errorf("unsupported file type %v", mode.Type())
	}
}

func hashContent(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Tools runs each command with sh -c in dir and returns its standard output
// and standard error.
func Tools(ctx context.Context, dir string, commands []string) ([]key.Tool, error) {
	tools := make([]key.Tool, 0, len(commands))
	for _, c := range commands {
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, "sh", "-c", c)
		cmd.Dir = dir
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				err = fmt.Errorf("%w: %s", err, msg)
			}
			return nil, fmt.Errorf("tool %q: %w", c, err)
		}
		tools = append(tools, key.Tool{Command: c, Stdout: stdout.String(), Stderr: stderr.String()})
	}
	return tools, nil
}

// Env reads each named variable with lookup.
func Env(names []string, lookup func(string) (string, bool)) []key.Var {
	vars := make([]key.Var, 0, len(names))
	for _, n := range names {
		v, ok := lookup(n)
		vars = append(vars, key.Var{Name: n, Value: v, Set: ok})
	}
	return vars
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], withStderr(err))
	}
	return string(out), nil
}

// withStderr adds a failed command's standard error to its error.
func withStderr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if msg := string(bytes.TrimSpace(ee.Stderr)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
	}
	return err
}
