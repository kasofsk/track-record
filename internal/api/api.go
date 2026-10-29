// Package api holds the store's wire types, which the server and the client
// share.
package api

import (
	"encoding/json"
	"regexp"
	"time"
)

var (
	namespacePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	digestPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidNamespace reports whether s is a namespace: one path segment of
// lowercase letters, digits, dots, underscores and hyphens.
func ValidNamespace(s string) bool { return namespacePattern.MatchString(s) }

// ValidDigest reports whether s is 64 lowercase hex characters, the form of a
// key and of a log's digest.
func ValidDigest(s string) bool { return digestPattern.MatchString(s) }

// Writer is the token that wrote a record.
type Writer struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
}

// Log refers to a stored log by its sha256 digest.
type Log struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// Record is a pass on record.
type Record struct {
	Statement  json.RawMessage `json:"statement"`
	Name       string          `json:"name"`
	Writer     Writer          `json:"writer"`
	RecordedAt time.Time       `json:"recordedAt"`
	ExpiresAt  time.Time       `json:"expiresAt"`
	Log        *Log            `json:"log"`
	DurationMs *int64          `json:"durationMs"` // how long the pass took to run, in milliseconds; nil if its writer did not say
}

// PutRecord is the body of a request to record a pass.
type PutRecord struct {
	Statement  json.RawMessage `json:"statement"`
	Log        *Log            `json:"log,omitempty"`
	DurationMs *int64          `json:"durationMs,omitempty"` // how long the pass took to run, in milliseconds
}

// Error is the body of every error response.
type Error struct {
	Error string `json:"error"`
}
