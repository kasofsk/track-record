// Package store keeps tokens, records and logs in SQLite.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/kasofsk/track-record/internal/api"
	_ "modernc.org/sqlite"
)

// Scope is what a token may do.
type Scope string

const (
	Read  Scope = "read"  // get records and logs
	Write Scope = "write" // get and put them
)

var (
	ErrNotFound = errors.New("not found")
	ErrNoLog    = errors.New("no log with that digest and size in the namespace")
)

// Token is a bearer credential for one namespace. Only its secret's sha256
// is stored.
type Token struct {
	ID        int64
	Label     string
	Namespace string
	Scope     Scope
	CreatedAt time.Time
	RevokedAt *time.Time
}

// Store is a SQLite database of tokens, records and logs. Times are stored
// as Unix seconds.
type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS tokens (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	hash       TEXT NOT NULL UNIQUE,
	label      TEXT NOT NULL,
	namespace  TEXT NOT NULL,
	scope      TEXT NOT NULL CHECK (scope IN ('read', 'write')),
	created_at INTEGER NOT NULL,
	revoked_at INTEGER
);
CREATE TABLE IF NOT EXISTS records (
	namespace    TEXT NOT NULL,
	key          TEXT NOT NULL,
	name         TEXT NOT NULL,
	statement    TEXT NOT NULL,
	writer_id    INTEGER NOT NULL REFERENCES tokens (id),
	writer_label TEXT NOT NULL,
	recorded_at  INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL,
	log_digest   TEXT,
	log_size     INTEGER,
	PRIMARY KEY (namespace, key)
);
CREATE INDEX IF NOT EXISTS records_by_expiry ON records (expires_at);
CREATE INDEX IF NOT EXISTS records_by_log ON records (namespace, log_digest);
CREATE TABLE IF NOT EXISTS logs (
	namespace  TEXT NOT NULL,
	digest     TEXT NOT NULL,
	bytes      BLOB NOT NULL,
	size       INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (namespace, digest)
);
`

// Open opens the database at path, creating it and its tables if need be.
func Open(path string) (*Store, error) {
	// SQLite writes the database, its WAL and its shared-memory file beside
	// path, and keeps its temporary tables and indices in memory, so nothing
	// else on the filesystem need be writable.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=temp_store(memory)")
	if err != nil {
		return nil, err
	}
	// One connection serialises this process's writes; the busy timeout
	// covers another process, such as a token command, writing alongside.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// CreateToken mints a token and returns its secret, which is not stored and
// cannot be recovered.
func (s *Store) CreateToken(ctx context.Context, namespace string, scope Scope, label string, now time.Time) (string, Token, error) {
	switch {
	case !api.ValidNamespace(namespace):
		return "", Token{}, fmt.Errorf("invalid namespace %q", namespace)
	case scope != Read && scope != Write:
		return "", Token{}, fmt.Errorf("invalid scope %q: want read or write", scope)
	case label == "":
		return "", Token{}, errors.New("a token needs a label")
	}
	b := make([]byte, 32)
	rand.Read(b)
	secret := "trk_" + base64.RawURLEncoding.EncodeToString(b)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (hash, label, namespace, scope, created_at) VALUES (?, ?, ?, ?, ?)`,
		hashSecret(secret), label, namespace, scope, now.Unix())
	if err != nil {
		return "", Token{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", Token{}, err
	}
	return secret, Token{ID: id, Label: label, Namespace: namespace, Scope: scope, CreatedAt: unix(now.Unix())}, nil
}

// Tokens lists every token, revoked ones included, oldest first.
func (s *Store) Tokens(ctx context.Context) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, label, namespace, scope, created_at, revoked_at FROM tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tokens []Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// RevokeToken revokes a token. Revoking a revoked token changes nothing.
func (s *Store) RevokeToken(ctx context.Context, id int64, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, now.Unix(), id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	var one int
	err = s.db.QueryRowContext(ctx, `SELECT 1 FROM tokens WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// Authenticate returns the live token whose secret this is.
func (s *Store) Authenticate(ctx context.Context, secret string) (Token, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx,
		`SELECT id, label, namespace, scope, created_at, revoked_at FROM tokens WHERE hash = ? AND revoked_at IS NULL`,
		hashSecret(secret)))
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	return t, err
}

// Record returns the live record for key in namespace.
func (s *Store) Record(ctx context.Context, namespace, key string, now time.Time) (api.Record, error) {
	r, err := scanRecord(s.db.QueryRowContext(ctx,
		`SELECT `+recordColumns+` FROM records WHERE namespace = ? AND key = ? AND expires_at > ?`,
		namespace, key, now.Unix()))
	if errors.Is(err, sql.ErrNoRows) {
		return api.Record{}, ErrNotFound
	}
	return r, err
}

// PutRecord records a pass written by writer, unless a live record for the
// key exists, in which case it returns that record unchanged; the first
// writer wins. The bool reports whether the record was created. A log must
// already be stored in the namespace.
func (s *Store) PutRecord(ctx context.Context, namespace, key, name string, statement []byte, log *api.Log, writer Token, now time.Time, ttl time.Duration) (api.Record, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Record{}, false, err
	}
	defer tx.Rollback()
	var digest sql.NullString
	var size sql.NullInt64
	if log != nil {
		var stored int64
		err := tx.QueryRowContext(ctx, `SELECT size FROM logs WHERE namespace = ? AND digest = ?`, namespace, log.Digest).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && stored != log.Size) {
			return api.Record{}, false, ErrNoLog
		}
		if err != nil {
			return api.Record{}, false, err
		}
		digest = sql.NullString{String: log.Digest, Valid: true}
		size = sql.NullInt64{Int64: log.Size, Valid: true}
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO records (namespace, key, name, statement, writer_id, writer_label, recorded_at, expires_at, log_digest, log_size)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (namespace, key) DO UPDATE SET
			name = excluded.name,
			statement = excluded.statement,
			writer_id = excluded.writer_id,
			writer_label = excluded.writer_label,
			recorded_at = excluded.recorded_at,
			expires_at = excluded.expires_at,
			log_digest = excluded.log_digest,
			log_size = excluded.log_size
		WHERE records.expires_at <= excluded.recorded_at`,
		namespace, key, name, string(statement), writer.ID, writer.Label, now.Unix(), now.Add(ttl).Unix(), digest, size)
	if err != nil {
		return api.Record{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return api.Record{}, false, err
	}
	r, err := scanRecord(tx.QueryRowContext(ctx, `SELECT `+recordColumns+` FROM records WHERE namespace = ? AND key = ?`, namespace, key))
	if err != nil {
		return api.Record{}, false, err
	}
	return r, n == 1, tx.Commit()
}

// PutLog stores a log under its digest, which the caller has checked. The
// bool reports whether it was not already stored.
func (s *Store) PutLog(ctx context.Context, namespace, digest string, data []byte, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO logs (namespace, digest, bytes, size, created_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT (namespace, digest) DO NOTHING`,
		namespace, digest, data, len(data), now.Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Log returns a stored log's bytes.
func (s *Store) Log(ctx context.Context, namespace, digest string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT bytes FROM logs WHERE namespace = ? AND digest = ?`, namespace, digest).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return data, err
}

// Sweep deletes expired records, then the logs that no live record refers to
// and that are older than grace. The grace covers a log uploaded just before
// the record that will refer to it.
func (s *Store) Sweep(ctx context.Context, now time.Time, grace time.Duration) (records, logs int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM records WHERE expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, 0, err
	}
	if records, err = res.RowsAffected(); err != nil {
		return 0, 0, err
	}
	res, err = tx.ExecContext(ctx, `
		DELETE FROM logs WHERE created_at <= ? AND NOT EXISTS (
			SELECT 1 FROM records r WHERE r.namespace = logs.namespace AND r.log_digest = logs.digest AND r.expires_at > ?
		)`, now.Add(-grace).Unix(), now.Unix())
	if err != nil {
		return 0, 0, err
	}
	if logs, err = res.RowsAffected(); err != nil {
		return 0, 0, err
	}
	return records, logs, tx.Commit()
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func unix(s int64) time.Time { return time.Unix(s, 0).UTC() }

type scanner interface{ Scan(...any) error }

func scanToken(row scanner) (Token, error) {
	var t Token
	var created int64
	var revoked sql.NullInt64
	if err := row.Scan(&t.ID, &t.Label, &t.Namespace, &t.Scope, &created, &revoked); err != nil {
		return Token{}, err
	}
	t.CreatedAt = unix(created)
	if revoked.Valid {
		r := unix(revoked.Int64)
		t.RevokedAt = &r
	}
	return t, nil
}

const recordColumns = `statement, name, writer_id, writer_label, recorded_at, expires_at, log_digest, log_size`

func scanRecord(row scanner) (api.Record, error) {
	var r api.Record
	var statement string
	var recorded, expires int64
	var digest sql.NullString
	var size sql.NullInt64
	if err := row.Scan(&statement, &r.Name, &r.Writer.ID, &r.Writer.Label, &recorded, &expires, &digest, &size); err != nil {
		return api.Record{}, err
	}
	r.Statement = []byte(statement)
	r.RecordedAt, r.ExpiresAt = unix(recorded), unix(expires)
	if digest.Valid {
		r.Log = &api.Log{Digest: digest.String, Size: size.Int64}
	}
	return r, nil
}
