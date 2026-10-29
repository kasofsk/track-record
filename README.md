# Track Record

Track Record keeps a record of commands that passed, each under a key computed
from what the command reads, and wraps a command so that a pass already on
record is reused instead of re-run. It needs git and a store, and nothing about
the project or the CI it runs in.

## Two rules

- **Only an exit of 0 is recorded.** Any other exit code passes through
  unchanged and is never stored.
- **Anything but a valid hit means the command runs.** A miss, an unreachable
  or slow store, a refused token, a malformed record, untracked files within
  the inputs or no store configured: the command runs, and nothing of the kind
  is ever a pass.

The exception is Track Record's own errors, which exit 2 without running the
command: a bad flag, or a bad value in a variable that stands for one; and,
with a store configured, no namespace, a server URL that is not one, no git, a
working directory outside a git checkout, or an `--inputs` pathspec that
matches no tracked file. A command that cannot be started exits 2 too.

## Install

```sh
CGO_ENABLED=0 go install -trimpath github.com/kasofsk/track-record/cmd/track-record@latest
```

The main package is `cmd/track-record`; it needs no cgo, so the binary is
static. From a clone, `CGO_ENABLED=0 go build -trimpath ./cmd/track-record`
builds the same.

## Run the server

```sh
docker build -t track-record .
docker run -d --name track-record -p 8080:8080 --read-only --tmpfs /tmp -v track-record-data:/data track-record
```

The image runs `/usr/local/bin/track-record serve` as uid 65532 and writes only
its database, in the volume `/data`, so its root can be read-only. Losing the
database loses the tokens along with the records: every token must be minted
again, and every pass earned again.

| Flag               | Environment                   | Default                                                              |
| ------------------ | ----------------------------- | -------------------------------------------------------------------- |
| `--listen`         | `TRACK_RECORD_LISTEN`         | `:8080`                                                              |
| `--metrics-listen` | `TRACK_RECORD_METRICS_LISTEN` | none: where to serve [metrics](#metrics), such as `:9090`            |
| `--db`             | `TRACK_RECORD_DB`             | `track-record.db`; in the image, `/data/track-record.db`             |
| `--ttl`            | `TRACK_RECORD_TTL`            | `720h`: how long a record lives after it was last recorded or reused |
| `--log-max`        | `TRACK_RECORD_LOG_MAX`        | `8388608` bytes                                                      |
| `--statement-max`  | `TRACK_RECORD_STATEMENT_MAX`  | `65536` bytes                                                        |

## Mint a token

```sh
docker exec track-record track-record token create --namespace myproject --scope write --label ci
```

It prints the token once, and nothing else on stdout. A `read` token looks
passes up; a `write` token records them too. A token reaches only its own
namespace. `token list` and `token revoke <id>` manage tokens; like `create`,
they work directly on the database `serve` has created, so they run where it
is. Revoking a token disowns every record it wrote.

## Wrap a command

In a git checkout:

```sh
export TRACK_RECORD_URL=http://localhost:8080 TRACK_RECORD_NAMESPACE=myproject TRACK_RECORD_TOKEN_FILE=$HOME/.track-record-token
(umask 077; docker exec track-record track-record token create --namespace myproject --scope write --label ci > "$TRACK_RECORD_TOKEN_FILE")
track-record run --name test -- go test ./...
```

The first run runs the command and records its pass. The next prints the
recorded log and `track-record: REUSED test (recorded <time> by ci)` instead,
until something in the key changes.

The token can come from `TRACK_RECORD_TOKEN` or `--token` instead, but those
put it where `ps` can show it, and a file does not. The command runs without
`TRACK_RECORD_TOKEN` or `TRACK_RECORD_TOKEN_FILE`, so a `track-record` inside
it runs plainly. It can still find the token, so the log is stored with each
copy of it replaced by `[token]`; the output on the terminal is left as it is.

The key covers the namespace, `--name`, the command line, the working directory
within the checkout, and every tracked file, or those `--inputs <pathspec>`
matches, as it is on disk: its content, type and executable bit. `--tool
'<command>'` adds a command's standard output and standard error, such as
`--tool 'java -version'`; `--env <name>` adds a variable's value, `--platform`
the OS and architecture, and `--salt` a salt.

The command runs with its key in `TRACK_RECORD_KEY`, so a randomized check
can seed from it and a reused pass stays reproducible; with no key, as when no
store is configured, the variable is unset.

Untracked files within the inputs mean no lookup and no record, since the
command may read files that will not be committed; `--allow-untracked` puts them
into the key instead. A pass is recorded only if the inputs, read again when the
command exits, still give the key it ran under, so an untracked file the command
writes within them blocks its record: a build's outputs must be ignored. An edit
made and undone before the command exits is not seen. `--fresh` skips the lookup
and still records a pass. `--timeout` caps each store request, 5s by default.

The key reads the tracked files in the working tree and nothing else: not the
files git ignores, through `.gitignore`, `.git/info/exclude` or
`core.excludesFile`; not the index, where staged content is; and not HEAD or any
other ref. A command that reads one must name it with `--tool`, or not be
wrapped: `--tool 'cat local.cfg'` for an ignored file, `--tool 'git write-tree'`
for a command that checks the index, such as a pre-commit hook, and `--tool 'git
rev-parse origin/main'` for one that compares against a ref.

`track-record key [flags] -- <command>` prints the key and a summary of what it
covers.

## Metrics

`serve --metrics-listen :9090` serves Prometheus metrics at `/metrics` on that
address, and nowhere else. They name every namespace and the commands on record
in it, which a token sees only for its own, so the API's port, which may be
public, does not serve them.

`run` records how long the command took with its pass, and the server counts:

| Series                                                              | Labels              | What it counts                                          |
| ------------------------------------------------------------------- | ------------------- | ------------------------------------------------------- |
| `track_record_hits_total`                                           | `namespace`, `name` | lookups that found a pass on record                     |
| `track_record_misses_total`                                         | `namespace`         | lookups that found none                                 |
| `track_record_saved_seconds_total`                                  | `namespace`, `name` | for each hit, how long the pass found took: time saved  |
| `track_record_passes_total`                                         | `namespace`, `name` | passes recorded, and those that found one already there |
| `track_record_pass_seconds_total`                                   | `namespace`, `name` | how long each of those passes took                      |
| `track_record_swept_records_total`, `track_record_swept_logs_total` |                     | what the sweeper deleted                                |
| `track_record_http_request_duration_seconds`                        | `route`, `code`     | each request, by how long it took                       |

The Go runtime's and the process's series are there too, with `go_build_info`.

```promql
sum(increase(track_record_saved_seconds_total[1d]))  # time saved in a day
sum(increase(track_record_pass_seconds_total[1d]))   # time spent earning passes
```

- `name` is `--name`, and each name is a series of its own, so it should name
  the command, such as `test`, and not vary from run to run.
- Time saved is how long the pass took when it was recorded; a hit still costs
  the key and the lookup. A pass recorded by a track-record that did not yet
  send its duration adds nothing to it.
- The server hears only of passes. A command that fails, or runs with no store,
  is not counted, so time spent on CI as a whole is the CI's to measure.
- The counters start from zero when the server does, which `rate` and
  `increase` allow for.

## Develop

```sh
just check
```
