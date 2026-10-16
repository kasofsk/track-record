package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/kasofsk/track-record/internal/store"
)

const tokenUsage = `Usage:
  track-record token create --namespace <ns> --scope read|write --label <who> [--db <path>]
  track-record token list [--db <path>]
  track-record token revoke [--db <path>] <id>

The token commands work directly on the database serve has created; there is
no admin API.
`

func token(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, tokenUsage)
		return 2
	}
	switch args[0] {
	case "create":
		return tokenCreate(args[1:], stdout, stderr)
	case "list":
		return tokenList(args[1:], stdout, stderr)
	case "revoke":
		return tokenRevoke(args[1:], stderr)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, tokenUsage)
		return 0
	}
	fmt.Fprintf(stderr, "track-record token: unknown command %q\n\n%s", args[0], tokenUsage)
	return 2
}

var dbEnv = []envFlag{{"db", "TRACK_RECORD_DB"}}

// tokenCreate writes exactly the token and a newline to stdout, so it can be
// piped into a secret, and everything else to stderr.
func tokenCreate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("token create", "token create [flags]", stderr)
	db := dbFlag(fs)
	namespace := fs.String("namespace", "", "the namespace the token is for (required)")
	scope := fs.String("scope", "", "read, to look records up, or write, to record them too (required)")
	label := fs.String("label", "", "who holds the token; stamped on what it records (required)")
	err := parse(fs, args, dbEnv)
	if err == nil && fs.NArg() > 0 {
		err = usageErrorf(fs, "unexpected argument %q", fs.Arg(0))
	}
	if err != nil {
		return exitCode(err)
	}
	st, err := openDB(*db, stderr)
	if err != nil {
		return 2
	}
	defer st.Close()
	secret, tok, err := st.CreateToken(context.Background(), *namespace, store.Scope(*scope), *label, time.Now())
	if err != nil {
		usageErrorf(fs, "%v", err)
		return 2
	}
	fmt.Fprintln(stdout, secret)
	fmt.Fprintf(stderr, "track-record: created token %d, %s for namespace %s, labelled %q; it is not shown again\n",
		tok.ID, tok.Scope, tok.Namespace, tok.Label)
	return 0
}

func tokenList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("token list", "token list [flags]", stderr)
	db := dbFlag(fs)
	if err := parse(fs, args, dbEnv); err != nil {
		return exitCode(err)
	}
	st, err := openDB(*db, stderr)
	if err != nil {
		return 2
	}
	defer st.Close()
	tokens, err := st.Tokens(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "track-record: %v\n", err)
		return 1
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAMESPACE\tSCOPE\tLABEL\tCREATED\tREVOKED")
	for _, t := range tokens {
		revoked := "-"
		if t.RevokedAt != nil {
			revoked = t.RevokedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Namespace, t.Scope, t.Label, t.CreatedAt.Format(time.RFC3339), revoked)
	}
	w.Flush()
	return 0
}

func tokenRevoke(args []string, stderr io.Writer) int {
	fs := newFlagSet("token revoke", "token revoke [flags] <id>", stderr)
	db := dbFlag(fs)
	if err := parse(fs, args, dbEnv); err != nil {
		return exitCode(err)
	}
	if fs.NArg() != 1 {
		usageErrorf(fs, "give one token id")
		return 2
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil {
		usageErrorf(fs, "invalid token id %q", fs.Arg(0))
		return 2
	}
	st, err := openDB(*db, stderr)
	if err != nil {
		return 2
	}
	defer st.Close()
	if err := st.RevokeToken(context.Background(), id, time.Now()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			err = fmt.Errorf("no token %d", id)
		}
		fmt.Fprintf(stderr, "track-record: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "track-record: token %d is revoked\n", id)
	return 0
}

// openDB opens an existing database, so a mistyped --db is an error rather
// than a new, empty database.
func openDB(path string, stderr io.Writer) (*store.Store, error) {
	st, err := store.OpenExisting(path)
	if err != nil {
		fmt.Fprintf(stderr, "track-record token: %v; check --db or TRACK_RECORD_DB (track-record serve creates the database)\n", err)
	}
	return st, err
}
