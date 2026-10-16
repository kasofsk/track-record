// Package key computes a record's key: the sha256 of a canonical, versioned
// preimage of everything a command's pass depends on.
//
// The preimage is a sequence of fields, each written as its decimal byte
// length, a colon, the bytes and a newline. It opens with Tag and continues
// with one section per input, each led by its label; a list is written as its
// count followed by its entries.
package key

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
)

// Tag leads every preimage. Any change to the encoding changes the tag.
const Tag = "track-record key v1"

// Kind is a path's file type as found on disk.
type Kind string

const (
	Regular    Kind = "file"
	Executable Kind = "exec"
	Symlink    Kind = "symlink"
	Deleted    Kind = "deleted"
)

// File is one path in the key.
type File struct {
	Path   string // relative to the repository root, slash-separated
	Kind   Kind
	SHA256 string // hex digest of the content, or of a symlink's target; empty when deleted
}

// Tool is a command whose standard output and standard error are part of
// the key, each on its own.
type Tool struct {
	Command string
	Stdout  string
	Stderr  string
}

// Var is an environment variable. An unset variable differs from an empty one.
type Var struct {
	Name  string
	Value string
	Set   bool
}

// Inputs is everything the key covers.
type Inputs struct {
	Namespace string
	Name      string
	Dir       string // the working directory relative to the repository root, "." at the root
	Argv      []string
	Files     []File // tracked paths, in git ls-files order
	Untracked []File // untracked paths admitted into the key
	Tools     []Tool
	Env       []Var
	Platform  string // "os/arch", or empty when not part of the key
	Salt      string
}

// Section is one labelled part of the preimage.
type Section struct {
	Label string
	Bytes []byte
}

// Canonical returns the inputs with tools sorted by command and variables by
// name, each without duplicates, so the order flags were given in does not
// change the key.
func (in Inputs) Canonical() Inputs {
	in.Tools = slices.Clone(in.Tools)
	slices.SortStableFunc(in.Tools, func(a, b Tool) int { return cmp.Compare(a.Command, b.Command) })
	in.Tools = slices.CompactFunc(in.Tools, func(a, b Tool) bool { return a.Command == b.Command })
	in.Env = slices.Clone(in.Env)
	slices.SortStableFunc(in.Env, func(a, b Var) int { return cmp.Compare(a.Name, b.Name) })
	in.Env = slices.CompactFunc(in.Env, func(a, b Var) bool { return a.Name == b.Name })
	return in
}

// Sections returns the preimage's sections in order.
func (in Inputs) Sections() []Section {
	in = in.Canonical()
	var platform []string
	if in.Platform != "" {
		platform = []string{in.Platform}
	}
	return []Section{
		section("namespace", func(e *encoder) { e.field(in.Namespace) }),
		section("name", func(e *encoder) { e.field(in.Name) }),
		section("dir", func(e *encoder) { e.field(in.Dir) }),
		section("argv", func(e *encoder) { e.list(in.Argv) }),
		section("files", func(e *encoder) { e.files(in.Files) }),
		section("untracked", func(e *encoder) { e.files(in.Untracked) }),
		section("tools", func(e *encoder) {
			e.count(len(in.Tools))
			for _, t := range in.Tools {
				e.field(t.Command)
				e.field(t.Stdout)
				e.field(t.Stderr)
			}
		}),
		section("env", func(e *encoder) {
			e.count(len(in.Env))
			for _, v := range in.Env {
				e.field(v.Name)
				if v.Set {
					e.field("set")
					e.field(v.Value)
				} else {
					e.field("unset")
				}
			}
		}),
		section("platform", func(e *encoder) { e.list(platform) }),
		section("salt", func(e *encoder) { e.field(in.Salt) }),
	}
}

// Preimage returns the bytes the key is the digest of.
func (in Inputs) Preimage() []byte {
	var e encoder
	e.field(Tag)
	for _, s := range in.Sections() {
		e.Write(s.Bytes)
	}
	return e.Bytes()
}

// Key returns the key: the lowercase hex sha256 of the preimage.
func (in Inputs) Key() string {
	return Digest(in.Preimage())
}

// Digest returns the lowercase hex sha256 of b.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type encoder struct{ bytes.Buffer }

func section(label string, body func(*encoder)) Section {
	var e encoder
	e.field(label)
	body(&e)
	return Section{Label: label, Bytes: e.Bytes()}
}

func (e *encoder) field(s string) {
	e.WriteString(strconv.Itoa(len(s)))
	e.WriteByte(':')
	e.WriteString(s)
	e.WriteByte('\n')
}

func (e *encoder) count(n int) { e.field(strconv.Itoa(n)) }

func (e *encoder) list(items []string) {
	e.count(len(items))
	for _, s := range items {
		e.field(s)
	}
}

func (e *encoder) files(files []File) {
	e.count(len(files))
	for _, f := range files {
		e.field(f.Path)
		e.field(string(f.Kind))
		e.field(f.SHA256)
	}
}
