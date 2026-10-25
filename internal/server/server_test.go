package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kasofsk/track-record/internal/api"
	"github.com/kasofsk/track-record/internal/key"
	"github.com/kasofsk/track-record/internal/statement"
	"github.com/kasofsk/track-record/internal/store"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type fixture struct {
	t      *testing.T
	store  *store.Store
	server *Server
	now    time.Time
	logs   bytes.Buffer
	tokens map[string]string // by name: readA, writeA, writeA2, writeB, revoked
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{t: t, store: st, now: t0, tokens: map[string]string{}}
	f.server = New(st, Config{
		TTL:          time.Hour,
		MaxLog:       1 << 10,
		MaxStatement: 4 << 10,
		Logger:       slog.New(slog.NewJSONHandler(&f.logs, nil)),
		Now:          func() time.Time { return f.now },
	})
	for name, spec := range map[string]struct {
		ns    string
		scope store.Scope
	}{
		"readA":   {"a", store.Read},
		"writeA":  {"a", store.Write},
		"writeA2": {"a", store.Write},
		"writeB":  {"b", store.Write},
		"revoked": {"a", store.Write},
	} {
		secret, tok, err := st.CreateToken(context.Background(), spec.ns, spec.scope, name, t0)
		if err != nil {
			t.Fatal(err)
		}
		if name == "revoked" {
			if err := st.RevokeToken(context.Background(), tok.ID, t0); err != nil {
				t.Fatal(err)
			}
		}
		f.tokens[name] = secret
	}
	return f
}

// do sends a request with the named token ("" for none) and returns the
// response.
func (f *fixture) do(method, path, token string, body []byte) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+f.tokens[token])
	}
	rec := httptest.NewRecorder()
	f.server.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) expect(method, path, token string, body []byte, want int) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := f.do(method, path, token, body)
	if rec.Code != want {
		f.t.Fatalf("%s %s as %q: %d %s, want %d", method, path, token, rec.Code, rec.Body, want)
	}
	return rec
}

// pass returns a key and a valid request body recording a pass for it.
func pass(t *testing.T, name string, log *api.Log, argv ...string) (string, []byte) {
	t.Helper()
	in := key.Inputs{Namespace: "a", Name: name, Dir: ".", Argv: argv}
	raw, err := json.Marshal(statement.New(in))
	if err != nil {
		t.Fatal(err)
	}
	return in.Key(), body(t, raw, log)
}

func body(t *testing.T, statement []byte, log *api.Log) []byte {
	t.Helper()
	b, err := json.Marshal(api.PutRecord{Statement: statement, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func decodeRecord(t *testing.T, rec *httptest.ResponseRecorder) api.Record {
	t.Helper()
	var r api.Record
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	return r
}

func TestScopesAndIsolation(t *testing.T) {
	f := newFixture(t)
	logData := []byte("ok\n")
	logPath := "/v1/logs/a/" + digest(logData)
	f.expect("PUT", logPath, "writeA", logData, http.StatusCreated)
	k, put := pass(t, "unit", &api.Log{Digest: digest(logData), Size: 3}, "true")
	recPath := "/v1/records/a/" + k
	f.expect("PUT", recPath, "writeA", put, http.StatusCreated)
	missing := "/v1/records/a/" + strings.Repeat("0", 64)
	f.tokens["unknown"] = "trk_" + strings.Repeat("A", 43)

	tests := []struct {
		method, path, token string
		body                []byte
		want                int
	}{
		{"GET", "/healthz", "", nil, http.StatusOK},
		{"GET", recPath, "", nil, http.StatusUnauthorized},
		{"GET", recPath, "unknown", nil, http.StatusUnauthorized},
		{"GET", recPath, "revoked", nil, http.StatusUnauthorized},
		{"PUT", recPath, "revoked", put, http.StatusUnauthorized},
		{"GET", logPath, "revoked", nil, http.StatusUnauthorized},
		{"GET", recPath, "readA", nil, http.StatusOK},
		{"GET", missing, "readA", nil, http.StatusNotFound},
		{"GET", logPath, "readA", nil, http.StatusOK},
		{"PUT", recPath, "readA", put, http.StatusForbidden},
		{"PUT", logPath, "readA", logData, http.StatusForbidden},
		{"GET", recPath, "writeA", nil, http.StatusOK},
		{"PUT", logPath, "writeA", logData, http.StatusOK},
		{"GET", recPath, "writeB", nil, http.StatusForbidden},
		{"GET", missing, "writeB", nil, http.StatusForbidden},
		{"PUT", recPath, "writeB", put, http.StatusForbidden},
		{"GET", logPath, "writeB", nil, http.StatusForbidden},
		{"PUT", logPath, "writeB", logData, http.StatusForbidden},
		{"GET", "/v1/records/b/" + k, "writeB", nil, http.StatusNotFound},
		{"GET", "/v1/logs/b/" + digest(logData), "writeB", nil, http.StatusNotFound},
		{"GET", "/v1/records/A/" + k, "writeA", nil, http.StatusForbidden},
	}
	for _, tt := range tests {
		f.expect(tt.method, tt.path, tt.token, tt.body, tt.want)
	}

	// A refusal reads the same whether or not the record exists.
	present, absent := f.do("GET", recPath, "writeB", nil), f.do("GET", missing, "writeB", nil)
	if present.Body.String() != absent.Body.String() {
		t.Errorf("refusals differ: %q and %q", present.Body, absent.Body)
	}
	present, absent = f.do("GET", recPath, "", nil), f.do("GET", missing, "", nil)
	if present.Body.String() != absent.Body.String() {
		t.Errorf("refusals differ: %q and %q", present.Body, absent.Body)
	}
}

func TestRecordRoundTrip(t *testing.T) {
	f := newFixture(t)
	k, put := pass(t, "unit", nil, "true")
	created := decodeRecord(t, f.expect("PUT", "/v1/records/a/"+k, "writeA", put, http.StatusCreated))
	got := decodeRecord(t, f.expect("GET", "/v1/records/a/"+k, "readA", nil, http.StatusOK))
	if got.Name != "unit" || got.Log != nil || !got.RecordedAt.Equal(t0) || !got.ExpiresAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("record = %+v", got)
	}
	if got.Writer.Label != "writeA" || got.Writer != created.Writer {
		t.Errorf("writer = %+v, want writeA's", got.Writer)
	}
	if _, err := statement.Parse(got.Statement, k); err != nil {
		t.Error(err)
	}
	var raw map[string]any
	json.Unmarshal(f.do("GET", "/v1/records/a/"+k, "readA", nil).Body.Bytes(), &raw)
	if v, ok := raw["log"]; !ok || v != nil {
		t.Errorf("log = %v, want null", v)
	}
}

func TestWriterIsTheToken(t *testing.T) {
	f := newFixture(t)
	in := key.Inputs{Namespace: "a", Name: "unit", Dir: ".", Argv: []string{"true"}}
	s := statement.New(in)
	raw, _ := json.Marshal(s)
	var m map[string]any
	json.Unmarshal(raw, &m)
	m["writer"] = map[string]any{"id": 999, "label": "someone else"}
	raw, _ = json.Marshal(m)
	b, _ := json.Marshal(map[string]any{"statement": json.RawMessage(raw), "writer": map[string]any{"id": 999, "label": "x"}})
	got := decodeRecord(t, f.expect("PUT", "/v1/records/a/"+in.Key(), "writeA", b, http.StatusCreated))
	if got.Writer.Label != "writeA" || got.Writer.ID == 999 {
		t.Errorf("writer = %+v, want the token that wrote it", got.Writer)
	}
}

func TestFirstWriterWins(t *testing.T) {
	f := newFixture(t)
	k, first := pass(t, "unit", nil, "true")
	f.expect("PUT", "/v1/records/a/"+k, "writeA", first, http.StatusCreated)

	// Another valid statement for the same key, from another writer.
	in := key.Inputs{Namespace: "a", Name: "unit", Dir: ".", Argv: []string{"true"}}
	s := statement.New(in)
	s.Predicate.Configuration[0].Annotations = map[string]any{"note": "second"}
	raw, _ := json.Marshal(s)
	f.now = t0.Add(time.Minute)
	again := decodeRecord(t, f.expect("PUT", "/v1/records/a/"+k, "writeA2", body(t, raw, nil), http.StatusOK))
	got := decodeRecord(t, f.expect("GET", "/v1/records/a/"+k, "readA", nil, http.StatusOK))
	for _, r := range []api.Record{again, got} {
		if r.Writer.Label != "writeA" || !r.RecordedAt.Equal(t0) || strings.Contains(string(r.Statement), "second") {
			t.Errorf("record changed: %+v", r)
		}
	}
}

func TestRevokedWritersRecordsStopCounting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	data := []byte("passed\n")
	f.expect("PUT", "/v1/logs/a/"+digest(data), "writeA", data, http.StatusCreated)
	k, put := pass(t, "unit", &api.Log{Digest: digest(data), Size: int64(len(data))}, "true")
	f.expect("PUT", "/v1/records/a/"+k, "writeA", put, http.StatusCreated)
	f.expect("GET", "/v1/records/a/"+k, "readA", nil, http.StatusOK)

	tokens, err := f.store.Tokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range tokens {
		if tok.Label == "writeA" {
			if err := f.store.RevokeToken(ctx, tok.ID, f.now); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.expect("GET", "/v1/records/a/"+k, "readA", nil, http.StatusNotFound)

	// Another writer can record the key again at once.
	f.now = t0.Add(time.Minute)
	rec := decodeRecord(t, f.expect("PUT", "/v1/records/a/"+k, "writeA2", put, http.StatusCreated))
	if rec.Writer.Label != "writeA2" {
		t.Errorf("writer = %+v, want writeA2", rec.Writer)
	}
	f.expect("GET", "/v1/records/a/"+k, "readA", nil, http.StatusOK)

	// A sweep deletes a revoked writer's records, and with them the hold
	// on their logs.
	other := []byte("other\n")
	f.expect("PUT", "/v1/logs/a/"+digest(other), "writeA2", other, http.StatusCreated)
	k2, put2 := pass(t, "lint", &api.Log{Digest: digest(other), Size: int64(len(other))}, "true")
	f.expect("PUT", "/v1/records/a/"+k2, "writeA2", put2, http.StatusCreated)
	for _, tok := range tokens {
		if tok.Label == "writeA2" {
			if err := f.store.RevokeToken(ctx, tok.ID, f.now); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.now = f.now.Add(LogGrace + time.Minute)
	if err := f.server.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Log(ctx, "a", digest(other)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the log of a revoked writer's record survived the sweep: %v", err)
	}
}

func TestStatementValidation(t *testing.T) {
	f := newFixture(t)
	logData := []byte("log")
	f.expect("PUT", "/v1/logs/a/"+digest(logData), "writeA", logData, http.StatusCreated)
	k, valid := pass(t, "unit", nil, "true")
	other, _ := pass(t, "unit", nil, "false")
	failed := func() []byte {
		in := key.Inputs{Namespace: "a", Name: "unit", Dir: ".", Argv: []string{"true"}}
		s := statement.New(in)
		s.Predicate.Result = "FAILED"
		raw, _ := json.Marshal(s)
		return body(t, raw, nil)
	}()
	tests := []struct {
		name string
		key  string
		body []byte
		want int
	}{
		{"valid", k, valid, http.StatusCreated},
		{"subject is another key", other, valid, http.StatusBadRequest},
		{"failed", k, failed, http.StatusBadRequest},
		{"not json", k, []byte("{"), http.StatusBadRequest},
		{"no statement", k, []byte(`{}`), http.StatusBadRequest},
		{"statement not an object", k, []byte(`{"statement": "x"}`), http.StatusBadRequest},
		{"key not hex", strings.Repeat("g", 64), valid, http.StatusBadRequest},
		{"key uppercase", strings.ToUpper(k), valid, http.StatusBadRequest},
		{"key short", k[:63], valid, http.StatusBadRequest},
		{"log absent", k, body(t, mustStatement(t, k, valid), &api.Log{Digest: digest([]byte("other")), Size: 5}), http.StatusBadRequest},
		{"log size wrong", k, body(t, mustStatement(t, k, valid), &api.Log{Digest: digest(logData), Size: 4}), http.StatusBadRequest},
		{"log digest malformed", k, body(t, mustStatement(t, k, valid), &api.Log{Digest: "x", Size: 3}), http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.t = t
			if tt.name != "valid" {
				// Validation comes first, so a valid record already there
				// does not turn a bad request into a 200.
				f.do("PUT", "/v1/records/a/"+k, "writeA", valid)
			}
			f.expect("PUT", "/v1/records/a/"+tt.key, "writeA", tt.body, tt.want)
		})
	}
}

func mustStatementOf(t *testing.T, b []byte) []byte {
	t.Helper()
	var r api.PutRecord
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r.Statement
}

func mustStatement(t *testing.T, k string, b []byte) []byte {
	t.Helper()
	var r api.PutRecord
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if _, err := statement.Parse(r.Statement, k); err != nil {
		t.Fatal(err)
	}
	return r.Statement
}

func TestLogs(t *testing.T) {
	f := newFixture(t)
	data := []byte("line one\nline two\n")
	path := "/v1/logs/a/" + digest(data)
	f.expect("GET", path, "readA", nil, http.StatusNotFound)
	f.expect("PUT", path, "writeA", data, http.StatusCreated)
	f.expect("PUT", path, "writeA", data, http.StatusOK)
	if got := f.expect("GET", path, "readA", nil, http.StatusOK).Body.Bytes(); !bytes.Equal(got, data) {
		t.Errorf("log = %q, want %q", got, data)
	}
	f.expect("PUT", path, "writeA", []byte("something else"), http.StatusBadRequest)
	f.expect("PUT", "/v1/logs/a/not-a-digest", "writeA", data, http.StatusBadRequest)
}

func TestSizeLimits(t *testing.T) {
	f := newFixture(t)
	atLimit := bytes.Repeat([]byte("x"), 1<<10)
	f.expect("PUT", "/v1/logs/a/"+digest(atLimit), "writeA", atLimit, http.StatusCreated)

	over := append(atLimit, 'x')
	f.expect("PUT", "/v1/logs/a/"+digest(over), "writeA", over, http.StatusRequestEntityTooLarge)
	// Without a Content-Length the limit holds while reading.
	req := httptest.NewRequest("PUT", "/v1/logs/a/"+digest(over), bytes.NewReader(over))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+f.tokens["writeA"])
	rec := httptest.NewRecorder()
	f.server.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("unsized log over the limit: %d", rec.Code)
	}

	// A statement just over the limit, in a body the envelope admits.
	_, put := pass(t, "unit", nil, "")
	pad := 4<<10 - len(mustStatementOf(t, put)) + 1
	k, put := pass(t, "unit", nil, strings.Repeat("a", pad))
	if n := len(mustStatementOf(t, put)); n != 4<<10+1 {
		t.Fatalf("statement is %d bytes", n)
	}
	f.expect("PUT", "/v1/records/a/"+k, "writeA", put, http.StatusRequestEntityTooLarge)
	k, put = pass(t, "unit", nil, strings.Repeat("a", pad-1))
	f.expect("PUT", "/v1/records/a/"+k, "writeA", put, http.StatusCreated)
	k, put = pass(t, "unit", nil, strings.Repeat("a", 8<<10))
	f.expect("PUT", "/v1/records/a/"+k, "writeA", put, http.StatusRequestEntityTooLarge)
}

// A lookup renews the record it returns, so one in use outlives its first
// ttl and the log it refers to survives a sweep after that.
func TestLookupRenewsRecord(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	l := []byte("log")
	f.expect("PUT", "/v1/logs/a/"+digest(l), "writeA", l, http.StatusCreated)
	k, put := pass(t, "unit", &api.Log{Digest: digest(l), Size: int64(len(l))}, "true")
	f.expect("PUT", "/v1/records/a/"+k, "writeA", put, http.StatusCreated)

	f.now = t0.Add(59 * time.Minute)
	f.expect("GET", "/v1/records/a/"+k, "readA", nil, http.StatusOK)
	f.now = t0.Add(90 * time.Minute)
	if err := f.server.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	rec := decodeRecord(t, f.expect("GET", "/v1/records/a/"+k, "readA", nil, http.StatusOK))
	f.expect("GET", "/v1/logs/a/"+digest(l), "readA", nil, http.StatusOK)
	if want := f.now.Add(time.Hour); !rec.ExpiresAt.Equal(want) {
		t.Errorf("expires %v, want %v", rec.ExpiresAt, want)
	}
}

func TestExpiryAndSweep(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	kept, swept, young := []byte("kept"), []byte("swept"), []byte("young")
	for _, l := range [][]byte{kept, swept} {
		f.expect("PUT", "/v1/logs/a/"+digest(l), "writeA", l, http.StatusCreated)
	}
	oldKey, oldPut := pass(t, "old", &api.Log{Digest: digest(swept), Size: int64(len(swept))}, "true")
	f.expect("PUT", "/v1/records/a/"+oldKey, "writeA", oldPut, http.StatusCreated)

	// At 59 minutes the record is live, which a put sees without a lookup
	// renewing it; at an hour it is gone.
	f.now = t0.Add(59 * time.Minute)
	f.expect("PUT", "/v1/records/a/"+oldKey, "writeA", oldPut, http.StatusOK)
	liveKey, livePut := pass(t, "live", &api.Log{Digest: digest(kept), Size: int64(len(kept))}, "true")
	f.expect("PUT", "/v1/records/a/"+liveKey, "writeA", livePut, http.StatusCreated)
	f.now = t0.Add(time.Hour)
	f.expect("GET", "/v1/records/a/"+oldKey, "readA", nil, http.StatusNotFound)

	// An expired record no longer blocks a new one, even before a sweep.
	f.now = t0.Add(61 * time.Minute)
	rec := decodeRecord(t, f.expect("PUT", "/v1/records/a/"+oldKey, "writeA2", oldPut, http.StatusCreated))
	if rec.Writer.Label != "writeA2" || !rec.RecordedAt.Equal(f.now) {
		t.Errorf("replacement = %+v", rec)
	}

	// Let the replacement expire too, and upload a log too young to sweep.
	f.now = t0.Add(3 * time.Hour)
	f.expect("PUT", "/v1/logs/a/"+digest(young), "writeA", young, http.StatusCreated)
	f.now = t0.Add(3*time.Hour + LogGrace - time.Minute)
	f.expect("GET", "/v1/records/a/"+liveKey, "readA", nil, http.StatusNotFound)
	if err := f.server.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	f.expect("GET", "/v1/logs/a/"+digest(swept), "readA", nil, http.StatusNotFound)
	f.expect("GET", "/v1/logs/a/"+digest(kept), "readA", nil, http.StatusNotFound)
	f.expect("GET", "/v1/logs/a/"+digest(young), "readA", nil, http.StatusOK)

	// A log a live record refers to survives any number of sweeps.
	freshKey, freshPut := pass(t, "fresh", &api.Log{Digest: digest(young), Size: int64(len(young))}, "true")
	f.expect("PUT", "/v1/records/a/"+freshKey, "writeA", freshPut, http.StatusCreated)
	f.now = f.now.Add(31 * time.Minute)
	if err := f.server.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	f.expect("GET", "/v1/logs/a/"+digest(young), "readA", nil, http.StatusOK)
	f.expect("GET", "/v1/records/a/"+freshKey, "readA", nil, http.StatusOK)
}

func TestRequestLogOmitsTokensAndBodies(t *testing.T) {
	f := newFixture(t)
	data := []byte("very distinctive log body")
	f.expect("PUT", "/v1/logs/a/"+digest(data), "writeA", data, http.StatusCreated)
	logged := f.logs.String()
	for _, secret := range f.tokens {
		if strings.Contains(logged, secret) {
			t.Error("a token was logged")
		}
	}
	if strings.Contains(logged, "distinctive") {
		t.Error("a body was logged")
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(strings.Split(logged, "\n")[0]), &line); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{
		"method":    "PUT",
		"route":     "PUT /v1/logs/{namespace}/{digest}",
		"namespace": "a",
		"status":    float64(201),
	} {
		if line[field] != want {
			t.Errorf("%s = %v, want %v", field, line[field], want)
		}
	}
	if _, ok := line["duration"]; !ok {
		t.Error("no duration")
	}
}
