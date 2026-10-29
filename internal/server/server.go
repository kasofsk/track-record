// Package server serves the store over HTTP. It treats a key as opaque: it
// checks a statement against the key it is put under, and never computes one.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kasofsk/track-record/internal/api"
	"github.com/kasofsk/track-record/internal/statement"
	"github.com/kasofsk/track-record/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

// Config is how the server treats what it stores.
type Config struct {
	TTL          time.Duration // how long a record lives after it was last recorded or reused
	MaxLog       int64         // bytes
	MaxStatement int64         // bytes
	Logger       *slog.Logger
	Metrics      prometheus.Registerer // where the server's series are registered; nil for nowhere
	Now          func() time.Time
}

// envelope is room in a record request's body for what surrounds the
// statement.
const envelope = 1 << 10

// LogGrace is how long a log no live record refers to is kept, so that a
// sweep cannot remove a log between its upload and the record that refers
// to it.
const LogGrace = time.Hour

// Server is the store's HTTP handler.
type Server struct {
	store   *store.Store
	cfg     Config
	mux     *http.ServeMux
	metrics *metrics
}

// New returns a server for st.
func New(st *store.Store, cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Server{store: st, cfg: cfg, mux: http.NewServeMux(), metrics: newMetrics(cfg.Metrics)}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.Handle("GET /v1/records/{namespace}/{key}", s.authorize(store.Read, s.getRecord))
	s.mux.Handle("PUT /v1/records/{namespace}/{key}", s.authorize(store.Write, s.putRecord))
	s.mux.Handle("GET /v1/logs/{namespace}/{digest}", s.authorize(store.Read, s.getLog))
	s.mux.Handle("PUT /v1/logs/{namespace}/{digest}", s.authorize(store.Write, s.putLog))
	return s
}

// ServeHTTP serves a request, then logs it, without its token or body, and
// measures it.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(sw, r)
	took := time.Since(start)
	s.cfg.Logger.Info("request",
		"method", r.Method,
		"route", r.Pattern,
		"namespace", r.PathValue("namespace"),
		"status", sw.status,
		"duration", took)
	s.metrics.request(r.Pattern, sw.status, took)
}

// Sweep deletes expired records and the logs nothing live refers to.
func (s *Server) Sweep(ctx context.Context) error {
	records, logs, err := s.store.Sweep(ctx, s.cfg.Now(), LogGrace)
	if err != nil {
		return err
	}
	s.cfg.Logger.Info("sweep", "records", records, "logs", logs)
	s.metrics.sweptRecords.Add(float64(records))
	s.metrics.sweptLogs.Add(float64(logs))
	return nil
}

// RunSweeper sweeps now and then at every interval until ctx is done.
func (s *Server) RunSweeper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
			s.cfg.Logger.Error("sweep", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type handler func(w http.ResponseWriter, r *http.Request, tok store.Token)

// authorize admits a request whose bearer token is live, is for the path's
// namespace and has the scope. It decides before anything is looked up, so a
// refusal says nothing about what the namespace holds.
func (s *Server) authorize(need store.Scope, h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, secret, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") || secret == "" {
			unauthorized(w)
			return
		}
		tok, err := s.store.Authenticate(r.Context(), secret)
		if errors.Is(err, store.ErrNotFound) {
			unauthorized(w)
			return
		}
		if err != nil {
			s.internal(w, err)
			return
		}
		if tok.Namespace != r.PathValue("namespace") || (need == store.Write && tok.Scope != store.Write) {
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		h(w, r, tok)
	})
}

func (s *Server) getRecord(w http.ResponseWriter, r *http.Request, tok store.Token) {
	key := r.PathValue("key")
	if !api.ValidDigest(key) {
		writeError(w, http.StatusBadRequest, "a key is 64 lowercase hex characters")
		return
	}
	rec, err := s.store.Record(r.Context(), tok.Namespace, key, s.cfg.Now(), s.cfg.TTL)
	if errors.Is(err, store.ErrNotFound) {
		s.metrics.miss(tok.Namespace)
		writeError(w, http.StatusNotFound, "no record")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	s.metrics.hit(tok.Namespace, rec.Name, rec.DurationMs)
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) putRecord(w http.ResponseWriter, r *http.Request, tok store.Token) {
	key := r.PathValue("key")
	if !api.ValidDigest(key) {
		writeError(w, http.StatusBadRequest, "a key is 64 lowercase hex characters")
		return
	}
	var body api.PutRecord
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.cfg.MaxStatement+envelope)).Decode(&body)
	if tooLarge(err) || int64(len(body.Statement)) > s.cfg.MaxStatement {
		writeError(w, http.StatusRequestEntityTooLarge, "the statement is over "+strconv.FormatInt(s.cfg.MaxStatement, 10)+" bytes")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "body: "+err.Error())
		return
	}
	st, err := statement.Parse(body.Statement, key)
	if err != nil {
		writeError(w, http.StatusBadRequest, "statement: "+err.Error())
		return
	}
	if body.Log != nil && (!api.ValidDigest(body.Log.Digest) || body.Log.Size < 0) {
		writeError(w, http.StatusBadRequest, "log: a digest is 64 lowercase hex characters and a size is not negative")
		return
	}
	if body.DurationMs != nil && *body.DurationMs < 0 {
		writeError(w, http.StatusBadRequest, "durationMs: a duration is not negative")
		return
	}
	name := st.Subject[0].Name
	rec, created, err := s.store.PutRecord(r.Context(), tok.Namespace, key, name, body.Statement, body.Log, body.DurationMs, tok, s.cfg.Now(), s.cfg.TTL)
	if errors.Is(err, store.ErrNoLog) {
		writeError(w, http.StatusBadRequest, "log: "+err.Error())
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	s.metrics.pass(tok.Namespace, name, body.DurationMs)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, rec)
}

func (s *Server) getLog(w http.ResponseWriter, r *http.Request, tok store.Token) {
	digest := r.PathValue("digest")
	if !api.ValidDigest(digest) {
		writeError(w, http.StatusBadRequest, "a digest is 64 lowercase hex characters")
		return
	}
	data, err := s.store.Log(r.Context(), tok.Namespace, digest)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no log")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

func (s *Server) putLog(w http.ResponseWriter, r *http.Request, tok store.Token) {
	digest := r.PathValue("digest")
	if !api.ValidDigest(digest) {
		writeError(w, http.StatusBadRequest, "a digest is 64 lowercase hex characters")
		return
	}
	tooBig := "the log is over " + strconv.FormatInt(s.cfg.MaxLog, 10) + " bytes"
	if r.ContentLength > s.cfg.MaxLog {
		writeError(w, http.StatusRequestEntityTooLarge, tooBig)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxLog))
	if tooLarge(err) {
		writeError(w, http.StatusRequestEntityTooLarge, tooBig)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "body: "+err.Error())
		return
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != digest {
		writeError(w, http.StatusBadRequest, "the body's sha256 is not the digest")
		return
	}
	created, err := s.store.PutLog(r.Context(), tok.Namespace, digest, data, s.cfg.Now())
	if err != nil {
		s.internal(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, api.Log{Digest: digest, Size: int64(len(data))})
}

func (s *Server) internal(w http.ResponseWriter, err error) {
	s.cfg.Logger.Error("internal error", "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "unauthorized")
}

func tooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, api.Error{Error: msg})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
