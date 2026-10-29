package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kasofsk/track-record/internal/server"
	"github.com/kasofsk/track-record/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const sweepInterval = time.Hour

// dbFlag adds --db, which serve and the token commands share.
func dbFlag(fs *flag.FlagSet) *string {
	return fs.String("db", "track-record.db", "the SQLite database's path ($TRACK_RECORD_DB)")
}

func serve(args []string, stderr io.Writer) int {
	fs := newFlagSet("serve", "serve [flags]", stderr)
	listen := fs.String("listen", ":8080", "the address to listen on ($TRACK_RECORD_LISTEN)")
	metricsListen := fs.String("metrics-listen", "", "the address to serve Prometheus metrics on, at /metrics; none if empty ($TRACK_RECORD_METRICS_LISTEN)")
	db := dbFlag(fs)
	ttl := fs.Duration("ttl", 30*24*time.Hour, "how long a record lives after it was last recorded or reused ($TRACK_RECORD_TTL)")
	logMax := fs.Int64("log-max", 8<<20, "the largest log stored, in bytes ($TRACK_RECORD_LOG_MAX)")
	statementMax := fs.Int64("statement-max", 64<<10, "the largest statement accepted, in bytes ($TRACK_RECORD_STATEMENT_MAX)")
	err := parse(fs, args, []envFlag{
		{"listen", "TRACK_RECORD_LISTEN"},
		{"metrics-listen", "TRACK_RECORD_METRICS_LISTEN"},
		{"db", "TRACK_RECORD_DB"},
		{"ttl", "TRACK_RECORD_TTL"},
		{"log-max", "TRACK_RECORD_LOG_MAX"},
		{"statement-max", "TRACK_RECORD_STATEMENT_MAX"},
	})
	if err == nil && fs.NArg() > 0 {
		err = usageErrorf(fs, "unexpected argument %q", fs.Arg(0))
	}
	if err == nil && (*ttl <= 0 || *logMax <= 0 || *statementMax <= 0) {
		err = usageErrorf(fs, "--ttl, --log-max and --statement-max must be positive")
	}
	if err != nil {
		return exitCode(err)
	}

	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	st, err := store.Open(*db)
	if err != nil {
		logger.Error("open database", "err", err)
		return 1
	}
	defer st.Close()
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewBuildInfoCollector(),
	)
	srv := server.New(st, server.Config{TTL: *ttl, MaxLog: *logMax, MaxStatement: *statementMax, Logger: logger, Metrics: reg})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		logger.Error("listen", "err", err)
		return 1
	}
	servers := []*http.Server{{Handler: srv, ReadHeaderTimeout: 10 * time.Second}}
	listeners := []net.Listener{ln}
	attrs := []any{"addr", ln.Addr().String(), "db", *db}
	// The metrics have a listener of their own, so that the port the API is
	// served on, which may be public, does not serve them: they name the
	// namespaces and the commands on record, which a token sees only for its
	// own namespace.
	if *metricsListen != "" {
		mln, err := net.Listen("tcp", *metricsListen)
		if err != nil {
			logger.Error("listen", "err", err)
			return 1
		}
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		servers = append(servers, &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second})
		listeners = append(listeners, mln)
		attrs = append(attrs, "metrics", mln.Addr().String())
	}
	go srv.RunSweeper(ctx, sweepInterval)
	served := make(chan error, len(servers))
	for i, hs := range servers {
		go func() { served <- hs.Serve(listeners[i]) }()
	}
	logger.Info("listening", attrs...)
	select {
	case err := <-served:
		logger.Error("serve", "err", err)
		return 1
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, hs := range servers {
		if err := hs.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("shutdown", "err", err)
			return 1
		}
	}
	logger.Info("stopped")
	return 0
}
