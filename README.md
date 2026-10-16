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
database loses nothing but time.

| Flag              | Environment                  | Default                                        |
| ----------------- | ---------------------------- | ---------------------------------------------- |
| `--listen`        | `TRACK_RECORD_LISTEN`        | `:8080`                                        |
| `--db`            | `TRACK_RECORD_DB`            | `track-record.db`; in the image, `/data/track-record.db` |
| `--ttl`           | `TRACK_RECORD_TTL`           | `720h`: how long a record lives                |
| `--log-max`       | `TRACK_RECORD_LOG_MAX`       | `8388608` bytes                                |
| `--statement-max` | `TRACK_RECORD_STATEMENT_MAX` | `65536` bytes                                  |

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
export TRACK_RECORD_URL=http://localhost:8080 TRACK_RECORD_NAMESPACE=myproject
export TRACK_RECORD_TOKEN=$(docker exec track-record track-record token create --namespace myproject --scope write --label ci)
track-record run --name test -- go test ./...
```

The first run runs the command and records its pass. The next prints the
recorded log and `track-record: REUSED test (recorded <time> by ci)` instead,
until something in the key changes. The token can also come from the file
`TRACK_RECORD_TOKEN_FILE` names. The command sees neither variable, so its log
cannot carry the token, and a `track-record` inside it runs plainly.

The key covers the namespace, `--name`, the command line, the working directory
within the checkout, and every tracked file, or those `--inputs <pathspec>`
matches, as it is on disk: its content, type and executable bit. `--tool
'<command>'` adds a command's standard output and standard error, such as
`--tool 'java -version'`; `--env <name>` adds a variable's value, `--platform`
the OS and architecture, and `--salt` a salt.

Untracked files within the inputs mean no lookup and no record, since the
command may read files that will not be committed; `--allow-untracked` puts them
into the key instead. `--fresh` skips the lookup and still records a pass.
`--timeout` caps each store request, 5s by default.

`track-record key [flags] -- <command>` prints the key and a summary of what it
covers.

## Develop

```sh
just check
```
