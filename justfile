# gofmt -l is empty, go vet is clean and go test -race passes.
# Exits 0 clean, 1 on a finding, 2 when it could not run.
check:
    #!/usr/bin/env bash
    set -uo pipefail
    if ! command -v go >/dev/null || ! command -v gofmt >/dev/null; then
        echo "check: could not run: go and gofmt must be on PATH" >&2
        exit 2
    fi
    if [ "$(go env CGO_ENABLED)" != 1 ]; then
        echo "check: could not run: the race detector needs cgo (CGO_ENABLED=1 and a C compiler)" >&2
        exit 2
    fi
    if ! go mod download; then
        echo "check: could not run: the modules could not be downloaded" >&2
        exit 2
    fi
    status=0
    unformatted=$(gofmt -l .) || status=1
    if [ -n "$unformatted" ]; then
        printf 'gofmt: not formatted:\n%s\n' "$unformatted"
        status=1
    fi
    go vet ./... || status=1
    go test -race ./... || status=1
    exit $status
