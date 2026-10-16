package inputs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kasofsk/track-record/internal/gittest"
	"github.com/kasofsk/track-record/internal/key"
)

func TestMain(m *testing.M) {
	gittest.Hermetic()
	os.Exit(m.Run())
}

type env map[string]string

func (e env) lookup(name string) (string, bool) {
	v, ok := e[name]
	return v, ok
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir := gittest.Repo(t, map[string]string{
		"README.md":   "hello\n",
		"src/main.go": "package main\n",
		"bin/run.sh":  "#!/bin/sh\n",
	})
	chmod(t, filepath.Join(dir, "bin/run.sh"), 0o755)
	if err := os.Symlink("README.md", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	gittest.Commit(t, dir)
	return dir
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func baseOptions() Options {
	return Options{Namespace: "acme", Name: "unit", Argv: []string{"make", "test"}}
}

func collect(t *testing.T, dir string, o Options, e env) Result {
	t.Helper()
	res, err := Collect(context.Background(), dir, o, e.lookup)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestEveryInputKindChangesTheKey(t *testing.T) {
	tests := []struct {
		name string
		base func(o *Options) // optional: the starting options
		// change alters the repository, the options or the environment.
		change func(t *testing.T, dir *string, o *Options, e env)
	}{
		{"content", nil, func(t *testing.T, dir *string, o *Options, e env) {
			gittest.Write(t, *dir, "README.md", "hello!\n")
		}},
		{"exec bit set", nil, func(t *testing.T, dir *string, o *Options, e env) {
			chmod(t, filepath.Join(*dir, "README.md"), 0o755)
		}},
		{"exec bit cleared", nil, func(t *testing.T, dir *string, o *Options, e env) {
			chmod(t, filepath.Join(*dir, "bin/run.sh"), 0o644)
		}},
		{"deleted file", nil, func(t *testing.T, dir *string, o *Options, e env) {
			if err := os.Remove(filepath.Join(*dir, "src/main.go")); err != nil {
				t.Fatal(err)
			}
		}},
		{"new tracked file", nil, func(t *testing.T, dir *string, o *Options, e env) {
			gittest.Write(t, *dir, "src/util.go", "package main\n")
			gittest.Git(t, *dir, "add", "src/util.go")
		}},
		{"symlink target", nil, func(t *testing.T, dir *string, o *Options, e env) {
			link := filepath.Join(*dir, "link")
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("src/main.go", link); err != nil {
				t.Fatal(err)
			}
		}},
		{"untracked file allowed in", nil, func(t *testing.T, dir *string, o *Options, e env) {
			gittest.Write(t, *dir, "notes.txt", "x\n")
			o.AllowUntracked = true
		}},
		{"narrower inputs", nil, func(t *testing.T, dir *string, o *Options, e env) {
			o.Pathspecs = []string{"src"}
		}},
		{"tool", nil, func(t *testing.T, dir *string, o *Options, e env) {
			o.Tools = []string{"echo one"}
		}},
		// The tools read README.md, which these inputs leave out, so only
		// the tool's output can change the key.
		{"tool output", toolReading("cat README.md"), func(t *testing.T, dir *string, o *Options, e env) {
			gittest.Write(t, *dir, "README.md", "hello!\n")
		}},
		{"tool output on stderr", toolReading("cat README.md >&2"), func(t *testing.T, dir *string, o *Options, e env) {
			gittest.Write(t, *dir, "README.md", "hello!\n")
		}},
		{"tool output moved to stderr", toolReading("cat README.md"), func(t *testing.T, dir *string, o *Options, e env) {
			o.Tools = []string{"cat README.md >&2"}
		}},
		{"env value", nil, func(t *testing.T, dir *string, o *Options, e env) {
			e["MODE"] = "fast"
		}},
		{"unset becomes empty", nil, func(t *testing.T, dir *string, o *Options, e env) {
			e["UNSET"] = ""
		}},
		{"empty becomes unset", nil, func(t *testing.T, dir *string, o *Options, e env) {
			delete(e, "EMPTY")
		}},
		{"platform", nil, func(t *testing.T, dir *string, o *Options, e env) {
			o.Platform = "linux/amd64"
		}},
		{"salt", nil, func(t *testing.T, dir *string, o *Options, e env) {
			o.Salt = "disown"
		}},
		{"argv", nil, func(t *testing.T, dir *string, o *Options, e env) {
			o.Argv = []string{"make", "check"}
		}},
		{"name", nil, func(t *testing.T, dir *string, o *Options, e env) {
			o.Name = "lint"
		}},
		{"namespace", nil, func(t *testing.T, dir *string, o *Options, e env) {
			o.Namespace = "other"
		}},
		{"working directory", nil, func(t *testing.T, dir *string, o *Options, e env) {
			*dir = filepath.Join(*dir, "src")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := newRepo(t)
			o := baseOptions()
			o.Env = []string{"MODE", "EMPTY", "UNSET"}
			e := env{"MODE": "slow", "EMPTY": ""}
			if tt.base != nil {
				tt.base(&o)
			}
			before := collect(t, dir, o, e).Inputs.Key()
			tt.change(t, &dir, &o, e)
			after := collect(t, dir, o, e).Inputs.Key()
			if before == after {
				t.Errorf("the key did not change")
			}
		})
	}
}

// toolReading returns options whose only input path is src and whose one
// tool is command.
func toolReading(command string) func(o *Options) {
	return func(o *Options) {
		o.Pathspecs = []string{"src"}
		o.Tools = []string{command}
	}
}

func TestKeyReadsTheWorkingTreeOnly(t *testing.T) {
	dir := newRepo(t)
	gittest.Write(t, dir, "README.md", "edited\n")
	before := collect(t, dir, baseOptions(), nil).Inputs.Key()
	gittest.Commit(t, dir)
	old := time.Unix(1e9, 0)
	if err := os.Chtimes(filepath.Join(dir, "README.md"), old, old); err != nil {
		t.Fatal(err)
	}
	if after := collect(t, dir, baseOptions(), nil).Inputs.Key(); after != before {
		t.Error("committing or touching a file changed the key")
	}
}

func TestSameTreeSameKeyFromTwoClones(t *testing.T) {
	a := newRepo(t)
	b := filepath.Join(t.TempDir(), "elsewhere", "clone")
	gittest.Git(t, a, "clone", "-q", a, b)
	for _, sub := range []string{".", "src"} {
		o := baseOptions()
		o.Tools = []string{"ls"}
		ka := collect(t, filepath.Join(a, sub), o, nil).Inputs.Key()
		kb := collect(t, filepath.Join(b, sub), o, nil).Inputs.Key()
		if ka != kb {
			t.Errorf("from %s: keys differ between clones: %s and %s", sub, ka, kb)
		}
	}
}

func TestFilesAsOnDisk(t *testing.T) {
	dir := newRepo(t)
	if err := os.Remove(filepath.Join(dir, "src/main.go")); err != nil {
		t.Fatal(err)
	}
	got := collect(t, filepath.Join(dir, "bin"), baseOptions(), nil).Inputs
	want := []key.File{
		{Path: "README.md", Kind: key.Regular, SHA256: key.Digest([]byte("hello\n"))},
		{Path: "bin/run.sh", Kind: key.Executable, SHA256: key.Digest([]byte("#!/bin/sh\n"))},
		{Path: "link", Kind: key.Symlink, SHA256: key.Digest([]byte("README.md"))},
		{Path: "src/main.go", Kind: key.Deleted},
	}
	if !slices.Equal(got.Files, want) {
		t.Errorf("files = %+v\nwant %+v", got.Files, want)
	}
	if got.Dir != "bin" {
		t.Errorf("dir = %q, want bin", got.Dir)
	}
}

func TestUntracked(t *testing.T) {
	dir := newRepo(t)
	gittest.Write(t, dir, ".gitignore", "*.log\n")
	gittest.Commit(t, dir)
	gittest.Write(t, dir, "src/new.go", "package main\n")
	gittest.Write(t, dir, "notes.txt", "x\n")
	gittest.Write(t, dir, "build.log", "ignored\n")

	tests := []struct {
		pathspecs []string
		want      []string
	}{
		{nil, []string{"notes.txt", "src/new.go"}},
		{[]string{"src"}, []string{"src/new.go"}},
		{[]string{"README.md"}, nil},
	}
	for _, tt := range tests {
		o := baseOptions()
		o.Pathspecs = tt.pathspecs
		if got := collect(t, dir, o, nil).Untracked; !slices.Equal(got, tt.want) {
			t.Errorf("pathspecs %q: untracked = %q, want %q", tt.pathspecs, got, tt.want)
		}
	}

	o := baseOptions()
	o.AllowUntracked = true
	res := collect(t, dir, o, nil)
	if len(res.Untracked) != 0 || len(res.Inputs.Untracked) != 2 {
		t.Errorf("allowed: blocking %q, in the key %+v", res.Untracked, res.Inputs.Untracked)
	}
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T) (string, Options)
		inputFail bool // the command may still run, unkeyed
	}{
		{"not a checkout", func(t *testing.T) (string, Options) {
			return t.TempDir(), baseOptions()
		}, false},
		{"pathspec matches nothing", func(t *testing.T) (string, Options) {
			o := baseOptions()
			o.Pathspecs = []string{"srcc"}
			return newRepo(t), o
		}, false},
		{"tool fails", func(t *testing.T) (string, Options) {
			o := baseOptions()
			o.Tools = []string{"exit 3"}
			return newRepo(t), o
		}, true},
		{"tracked path is a directory", func(t *testing.T) (string, Options) {
			dir := newRepo(t)
			if err := os.Remove(filepath.Join(dir, "README.md")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, "README.md"), 0o755); err != nil {
				t.Fatal(err)
			}
			return dir, baseOptions()
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, o := tt.setup(t)
			_, err := Collect(context.Background(), dir, o, env{}.lookup)
			if err == nil {
				t.Fatal("no error")
			}
			var ie *InputError
			if errors.As(err, &ie) != tt.inputFail {
				t.Errorf("error %v: InputError = %v, want %v", err, !tt.inputFail, tt.inputFail)
			}
		})
	}
}
