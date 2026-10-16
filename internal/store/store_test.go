package store

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
