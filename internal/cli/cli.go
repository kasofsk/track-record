// Package cli is the track-record command line.
package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
)

const usage = `Track Record keeps passes on record, and wraps a command so that a pass
already on record is reused instead of re-run.

Usage:
  track-record run [flags] -- <command> [args...]   run a command, or reuse its pass
  track-record key [flags] -- <command> [args...]   print the key and what it covers
  track-record serve [flags]                        serve the store
  track-record token create|list|revoke [flags]     manage tokens in the database
  track-record --version

Run "track-record <command> -h" for a command's flags.
`

// Main runs the command line and returns the exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "run":
		return run(args[1:], stdin, stdout, stderr)
	case "key":
		return keyCommand(args[1:], stdout, stderr)
	case "serve":
		return serve(args[1:], stderr)
	case "token":
		return token(args[1:], stdout, stderr)
	case "--version", "-version":
		fmt.Fprintln(stdout, "track-record", Version())
		return 0
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "track-record: unknown command %q\n\n%s", args[0], usage)
	return 2
}

// Version is the module version the binary was built from.
func Version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}

// envFlag ties a flag to the environment variable that sets it when the
// command line does not.
type envFlag struct{ flag, env string }

// parse parses args and then sets each flag the command line left alone from
// its environment variable, if that is set and not empty.
func parse(fs *flag.FlagSet, args []string, env []envFlag) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	for _, e := range env {
		if v := os.Getenv(e.env); v != "" && !given[e.flag] {
			if err := fs.Set(e.flag, v); err != nil {
				return usageErrorf(fs, "%s=%q: %v", e.env, v, err)
			}
		}
	}
	return nil
}

// newFlagSet returns a flag set whose -h prints synopsis and the flags.
func newFlagSet(name, synopsis string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: track-record %s\n\nFlags:\n", synopsis)
		fs.PrintDefaults()
	}
	return fs
}

// usageErrorf reports a bad invocation, which the caller turns into exit 2.
func usageErrorf(fs *flag.FlagSet, format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	fmt.Fprintf(fs.Output(), "track-record %s: %v\n", fs.Name(), err)
	return err
}

// exitCode is the exit code for a parse error: 0 for -h, 2 otherwise.
func exitCode(err error) int {
	if err == flag.ErrHelp {
		return 0
	}
	return 2
}

// list is a repeatable string flag.
type list []string

func (l *list) String() string     { return strings.Join(*l, ", ") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }
