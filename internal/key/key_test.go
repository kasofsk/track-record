package key

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden preimage")

func fixture() Inputs {
	return Inputs{
		Namespace: "acme",
		Name:      "unit",
		Dir:       "pkg",
		Argv:      []string{"go", "test", "./..."},
		Files: []File{
			{Path: "README.md", Kind: Regular, SHA256: Digest([]byte("hello\n"))},
			{Path: "bin/run", Kind: Executable, SHA256: Digest([]byte("#!/bin/sh\n"))},
			{Path: "gone.txt", Kind: Deleted},
			{Path: "link", Kind: Symlink, SHA256: Digest([]byte("README.md"))},
		},
		Untracked: []File{{Path: "scratch.txt", Kind: Regular, SHA256: Digest(nil)}},
		Tools:     []Tool{{Command: "java -version", Stdout: "", Stderr: "openjdk version \"21.0.4\"\n"}},
		Env: []Var{
			{Name: "CI", Value: "true", Set: true},
			{Name: "EMPTY", Set: true},
			{Name: "UNSET"},
		},
		Platform: "linux/amd64",
		Salt:     "s1",
	}
}

const fixtureKey = "cd55bdc8c2953af9f2991f65cc81de62f348b05f7d141e17af456eae1ae062d8"

func TestGoldenPreimage(t *testing.T) {
	golden := filepath.Join("testdata", "preimage.golden")
	got := fixture().Preimage()
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("preimage differs from %s\ngot:\n%s", golden, got)
	}
	if k := fixture().Key(); k != fixtureKey {
		t.Errorf("key = %s, want %s", k, fixtureKey)
	}
}

func TestEveryInputChangesTheKey(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Inputs)
	}{
		{"namespace", func(in *Inputs) { in.Namespace = "other" }},
		{"name", func(in *Inputs) { in.Name = "lint" }},
		{"dir", func(in *Inputs) { in.Dir = "." }},
		{"argv", func(in *Inputs) { in.Argv = []string{"go", "test", "./...", "-race"} }},
		{"argv boundary", func(in *Inputs) { in.Argv = []string{"go", "test ./..."} }},
		{"content", func(in *Inputs) { in.Files[0].SHA256 = Digest([]byte("hello!\n")) }},
		{"exec bit", func(in *Inputs) { in.Files[1].Kind = Regular }},
		{"deleted", func(in *Inputs) { in.Files[0] = File{Path: "README.md", Kind: Deleted} }},
		{"new file", func(in *Inputs) { in.Files = append(in.Files, File{Path: "z", Kind: Regular, SHA256: Digest(nil)}) }},
		{"path", func(in *Inputs) { in.Files[0].Path = "README" }},
		{"symlink target", func(in *Inputs) { in.Files[3].SHA256 = Digest([]byte("bin/run")) }},
		{"tracked becomes untracked", func(in *Inputs) {
			in.Files, in.Untracked = in.Files[:3], append(in.Untracked, in.Files[3])
		}},
		{"untracked", func(in *Inputs) { in.Untracked = nil }},
		{"tool stderr", func(in *Inputs) { in.Tools[0].Stderr = "openjdk version \"25\"\n" }},
		{"tool stdout", func(in *Inputs) { in.Tools[0].Stdout = "x" }},
		{"tool output moves to stdout", func(in *Inputs) { in.Tools[0].Stdout, in.Tools[0].Stderr = in.Tools[0].Stderr, "" }},
		{"tool command", func(in *Inputs) { in.Tools[0].Command = "java --version" }},
		{"tool added", func(in *Inputs) { in.Tools = append(in.Tools, Tool{Command: "true"}) }},
		{"env value", func(in *Inputs) { in.Env[0].Value = "false" }},
		{"empty becomes unset", func(in *Inputs) { in.Env[1].Set = false }},
		{"unset becomes empty", func(in *Inputs) { in.Env[2].Set = true }},
		{"env added", func(in *Inputs) { in.Env = append(in.Env, Var{Name: "HOME"}) }},
		{"platform", func(in *Inputs) { in.Platform = "darwin/arm64" }},
		{"platform off", func(in *Inputs) { in.Platform = "" }},
		{"salt", func(in *Inputs) { in.Salt = "s2" }},
		{"salt off", func(in *Inputs) { in.Salt = "" }},
	}
	base := fixture().Key()
	seen := map[string]string{base: "base"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := fixture()
			tt.change(&in)
			k := in.Key()
			if prev, ok := seen[k]; ok {
				t.Fatalf("key %s is the same as for %q", k, prev)
			}
			seen[k] = tt.name
		})
	}
}

func TestFlagOrderDoesNotChangeTheKey(t *testing.T) {
	in := fixture()
	in.Tools = append(in.Tools, Tool{Command: "a", Stdout: "1"})
	in.Env = append(in.Env, Var{Name: "A", Value: "x", Set: true})
	reordered := in
	reordered.Tools = []Tool{in.Tools[1], in.Tools[0], in.Tools[1]}
	reordered.Env = []Var{in.Env[3], in.Env[2], in.Env[1], in.Env[0], in.Env[3]}
	if in.Key() != reordered.Key() {
		t.Error("reordering or repeating tools and variables changed the key")
	}
}
