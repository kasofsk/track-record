package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kasofsk/track-record/internal/api"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTokens(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	secret, tok, err := s.CreateToken(ctx, "acme", Write, "ci", t0)
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := strings.CutPrefix(secret, "trk_")
	if b, err := base64.RawURLEncoding.DecodeString(raw); !ok || err != nil || len(b) != 32 {
		t.Errorf("secret %q is not trk_ and 32 bytes of base64url", secret)
	}
	got, err := s.Authenticate(ctx, secret)
	if err != nil || got.ID != tok.ID || got.Namespace != "acme" || got.Scope != Write || got.Label != "ci" {
		t.Errorf("Authenticate = %+v, %v", got, err)
	}
	if _, err := s.Authenticate(ctx, secret+"x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a wrong secret: %v", err)
	}

	if err := s.RevokeToken(ctx, tok.ID, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeToken(ctx, tok.ID, t0.Add(time.Hour)); err != nil {
		t.Errorf("revoking twice: %v", err)
	}
	if err := s.RevokeToken(ctx, 999, t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking an unknown token: %v", err)
	}
	if _, err := s.Authenticate(ctx, secret); !errors.Is(err, ErrNotFound) {
		t.Errorf("a revoked token authenticates: %v", err)
	}
	tokens, err := s.Tokens(ctx)
	if err != nil || len(tokens) != 1 || tokens[0].RevokedAt == nil || !tokens[0].RevokedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("Tokens = %+v, %v", tokens, err)
	}
}

func TestCreateTokenRejects(t *testing.T) {
	s := open(t)
	tests := []struct {
		namespace string
		scope     Scope
		label     string
	}{
		{"", Read, "x"},
		{"Acme", Read, "x"},
		{"a/b", Read, "x"},
		{".a", Read, "x"},
		{strings.Repeat("a", 129), Read, "x"},
		{"acme", "admin", "x"},
		{"acme", Read, ""},
	}
	for _, tt := range tests {
		if _, _, err := s.CreateToken(context.Background(), tt.namespace, tt.scope, tt.label, t0); err == nil {
			t.Errorf("CreateToken(%q, %q, %q) succeeded", tt.namespace, tt.scope, tt.label)
		}
	}
	if _, _, err := s.CreateToken(context.Background(), strings.Repeat("a", 128), Read, "x", t0); err != nil {
		t.Errorf("a 128-character namespace: %v", err)
	}
}

func TestWritesOnlyBesideTheDatabase(t *testing.T) {
	s := open(t)
	for pragma, want := range map[string]string{"journal_mode": "wal", "temp_store": "2", "foreign_keys": "1"} {
		var got string
		if err := s.db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s = %s, want %s", pragma, got, want)
		}
	}
}

// The server records passes while a token command, a second process with
// its own connection, writes to the same database.
func TestWritesAlongsideAnotherConnection(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	server, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	admin, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	_, tok, err := server.CreateToken(ctx, "acme", Write, "ci", t0)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("ok\n")
	d := sha256.Sum256(data)
	digest := hex.EncodeToString(d[:])
	if _, err := server.PutLog(ctx, "acme", digest, data, t0); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	adminErr := make(chan error, 1)
	go func() {
		defer close(adminErr)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, _, err := admin.CreateToken(ctx, "acme", Read, fmt.Sprint("t", i), t0); err != nil {
				adminErr <- err
				return
			}
		}
	}()
	var failed int
	var first error
	for i := range 1000 {
		_, _, err := server.PutRecord(ctx, "acme", fmt.Sprintf("%064x", i), "unit", []byte(`{}`), &api.Log{Digest: digest, Size: int64(len(data))}, tok, t0, time.Hour)
		if err != nil {
			if failed++; first == nil {
				first = err
			}
		}
	}
	close(stop)
	if err := <-adminErr; err != nil {
		t.Errorf("creating tokens: %v", err)
	}
	if failed > 0 {
		t.Errorf("%d of 1000 records failed alongside another writer; the first: %v", failed, first)
	}
}

// A log uploaded again, by a run whose output matches an earlier one, is
// renewed, so a sweep between the upload and the run's record spares it.
func TestLogStoredAgainIsRenewed(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	_, tok, err := s.CreateToken(ctx, "acme", Write, "ci", t0)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("ok\n")
	d := sha256.Sum256(data)
	digest := hex.EncodeToString(d[:])
	if created, err := s.PutLog(ctx, "acme", digest, data, t0); err != nil || !created {
		t.Fatalf("first upload: created %v, %v", created, err)
	}
	now := t0.Add(2 * time.Hour)
	if created, err := s.PutLog(ctx, "acme", digest, data, now); err != nil || created {
		t.Fatalf("second upload: created %v, %v", created, err)
	}
	// An upload stamped earlier does not undo the renewal.
	if _, err := s.PutLog(ctx, "acme", digest, data, t0); err != nil {
		t.Fatal(err)
	}
	if _, logs, err := s.Sweep(ctx, now.Add(time.Second), time.Hour); err != nil || logs != 0 {
		t.Fatalf("sweep: %d logs deleted, %v", logs, err)
	}
	if _, _, err := s.PutRecord(ctx, "acme", strings.Repeat("a", 64), "unit", []byte(`{}`), &api.Log{Digest: digest, Size: int64(len(data))}, tok, now.Add(2*time.Second), time.Hour); err != nil {
		t.Errorf("record: %v", err)
	}
}

// recorded stores a pass for key written by a token of label at t0 with a
// one hour ttl.
func recorded(t *testing.T, s *Store, label string) (Token, string) {
	t.Helper()
	_, tok, err := s.CreateToken(context.Background(), "acme", Write, label, t0)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("a", 64)
	if _, _, err := s.PutRecord(context.Background(), "acme", key, "unit", []byte(`{}`), nil, tok, t0, time.Hour); err != nil {
		t.Fatal(err)
	}
	return tok, key
}

// A lookup that hits renews the record to a ttl after the lookup, so a record
// in use outlives the ttl it was recorded with.
func TestRecordHitRenews(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	_, key := recorded(t, s, "ci")
	t1 := t0.Add(50 * time.Minute)
	r, err := s.Record(ctx, "acme", key, t1, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if want := t1.Add(time.Hour); !r.ExpiresAt.Equal(want) {
		t.Errorf("expires %v, want %v", r.ExpiresAt, want)
	}
	// After the original expiry, before the renewed one.
	if _, err := s.Record(ctx, "acme", key, t0.Add(90*time.Minute), time.Hour); err != nil {
		t.Errorf("lookup after the original expiry: %v", err)
	}
}

// A lookup never shortens a record that already lives longer.
func TestRecordHitDoesNotShorten(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	_, key := recorded(t, s, "ci")
	r, err := s.Record(ctx, "acme", key, t0.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := t0.Add(time.Hour); !r.ExpiresAt.Equal(want) {
		t.Errorf("expires %v, want %v", r.ExpiresAt, want)
	}
}

func TestRecordNotLookedUpLapses(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	_, key := recorded(t, s, "ci")
	if _, err := s.Record(ctx, "acme", key, t0.Add(30*time.Minute), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(ctx, "acme", key, t0.Add(30*time.Minute).Add(time.Hour), time.Hour); !errors.Is(err, ErrNotFound) {
		t.Errorf("lookup a ttl after the last: %v, want ErrNotFound", err)
	}
}

func TestRecordOfRevokedWriterIsNotRenewed(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	tok, key := recorded(t, s, "ci")
	if err := s.RevokeToken(ctx, tok.ID, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(30 * time.Minute)
	if _, err := s.Record(ctx, "acme", key, t1, time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lookup: %v, want ErrNotFound", err)
	}
	if got := expiry(t, s, key); !got.Equal(t0.Add(time.Hour)) {
		t.Errorf("expires %v, want it unrenewed at %v", got, t0.Add(time.Hour))
	}
}

func TestExpiredRecordIsNotRenewed(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	_, key := recorded(t, s, "ci")
	if _, err := s.Record(ctx, "acme", key, t0.Add(2*time.Hour), time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lookup: %v, want ErrNotFound", err)
	}
	if got := expiry(t, s, key); !got.Equal(t0.Add(time.Hour)) {
		t.Errorf("expires %v, want it unrenewed at %v", got, t0.Add(time.Hour))
	}
	// Nor does it come back for a later lookup.
	if _, err := s.Record(ctx, "acme", key, t0.Add(2*time.Hour+time.Minute), time.Hour); !errors.Is(err, ErrNotFound) {
		t.Errorf("later lookup: %v, want ErrNotFound", err)
	}
}

// expiry reads a record's stored expiry whether or not it is live.
func expiry(t *testing.T, s *Store, key string) time.Time {
	t.Helper()
	var e int64
	if err := s.db.QueryRow(`SELECT expires_at FROM records WHERE key = ?`, key).Scan(&e); err != nil {
		t.Fatal(err)
	}
	return unix(e)
}
