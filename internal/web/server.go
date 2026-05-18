// Package web implements the Hearth 2.0 web UI backend for the forge daemon.
//
// The package serves a small chi-based HTTP server that exposes read-only
// JSON endpoints mirroring the existing IPC commands (status, queue,
// workers) plus a bcrypt-validated session login. It is intended to run
// in-process inside the daemon (no extra socket hop) and is gated by the
// FORGE_WEB_ENABLED environment variable.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/Robin831/Forge/internal/forgechat"
	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
)

// CommandHandler is the in-process dispatcher used by the web layer to call
// daemon command handlers without going through the IPC socket. The daemon
// passes its own handleIPC method here.
type CommandHandler func(cmd ipc.Command) ipc.Response

// Config holds the runtime configuration for the web server. Most fields are
// derived from environment variables in NewConfigFromEnv.
type Config struct {
	// Addr is the TCP listen address (e.g. ":8080"). Required.
	Addr string

	// Users maps username -> bcrypt hash. May be empty, in which case the
	// /login endpoint always rejects.
	Users map[string]string

	// SessionTTL is the sliding session lifetime. Defaults to 30 days when
	// zero.
	SessionTTL time.Duration

	// CookieName is the session cookie name. Defaults to "forge_session".
	CookieName string

	// CookieSecure forces the Secure cookie attribute. Defaults to false so
	// local development over plain HTTP works; production deployments behind
	// HTTPS should set this to true.
	CookieSecure bool

	// PurgeInterval controls how often expired session rows are purged from
	// the DB. Defaults to 1 hour. Set to a negative value to disable.
	PurgeInterval time.Duration
}

// AnvilLister returns the registered anvils as a map of name -> on-disk
// path. Callers use this for the Beads-Forge bead-emission flow: the web
// layer passes the names to claude (so it knows which anvils to target) and
// resolves names to paths when shelling out to bd. Implementations must be
// safe to call concurrently — the daemon's hot-reload may swap the config
// underneath.
type AnvilLister func() map[string]string

// AnvilDispatchTagLister returns the per-anvil auto-dispatch tag from
// forge.yaml (`auto_dispatch_tag`) as a map of name -> tag. Anvils with no
// tag configured are omitted from the map. Used by the Hearth web UI's
// one-click "Apply tag" action so the dispatch label is resolved
// server-side and matches the daemon's runtime config (including
// hot-reloads). Implementations must be safe to call concurrently.
type AnvilDispatchTagLister func() map[string]string

// BdRunnerFn is a process-spawning function compatible with the materializer
// in package forgechat. The daemon supplies forgechat.DefaultBdRunner; tests
// inject a fake to avoid spawning real bd subprocesses.
type BdRunnerFn = forgechat.BdRunner

// Server is the chi-based HTTP server. Construct with New and run with
// Start.
type Server struct {
	cfg     Config
	db      *state.DB
	handler CommandHandler
	logger  *slog.Logger

	httpServer *http.Server

	// chatRunner backs the Beads-Forge per-turn AI loop. Optional: when nil
	// the /api/forge/sessions/{id}/turn endpoint reports 503 so operators
	// know they need to configure a provider before relying on the page.
	chatRunner forgechat.Runner

	// anvils returns the live anvil registry. Optional: when nil the
	// /api/forge/sessions/{id}/create-beads endpoint reports 503 because
	// emission cannot proceed without anvil routing.
	anvils AnvilLister

	// anvilTags returns the live per-anvil dispatch tags. Optional: when
	// nil the /api/queue/{id}/apply-dispatch-tag endpoint reports 400 with
	// "anvil has no auto_dispatch_tag configured" for every request.
	anvilTags AnvilDispatchTagLister

	// bdRunner runs `bd` subprocesses for bead materialisation. Optional:
	// nil falls back to forgechat.DefaultBdRunner. Tests inject a fake.
	bdRunner BdRunnerFn

	// staticH serves the embedded SPA bundle. Built once in routes() so
	// handleLoginPage can fall back to it without re-walking the embedded
	// filesystem on every request.
	staticH http.HandlerFunc
}

// SetChatRunner installs the AI runner used by the Beads-Forge page. The
// daemon constructs the runner from its provider chain after web.New
// returns, so this is wired in via a setter rather than the Config struct.
// nil clears the runner.
func (s *Server) SetChatRunner(r forgechat.Runner) {
	s.chatRunner = r
}

// SetAnvilLister installs the registry callback used by the Beads-Forge
// bead-emission flow. The daemon snapshots its current config on each call
// so hot-reloads are picked up automatically.
func (s *Server) SetAnvilLister(a AnvilLister) {
	s.anvils = a
}

// SetAnvilDispatchTagLister installs the per-anvil dispatch tag callback
// used by the one-click Apply-tag action on the Hearth queue. nil clears
// the callback, after which apply-dispatch-tag requests are rejected.
func (s *Server) SetAnvilDispatchTagLister(a AnvilDispatchTagLister) {
	s.anvilTags = a
}

// SetBdRunner installs the bd subprocess shim used for bead materialisation.
// nil restores the default (forgechat.DefaultBdRunner).
func (s *Server) SetBdRunner(r BdRunnerFn) {
	s.bdRunner = r
}

// New constructs a Server. The cfg is validated; an error is returned when
// required fields are missing.
func New(cfg Config, db *state.DB, handler CommandHandler, logger *slog.Logger) (*Server, error) {
	if cfg.Addr == "" {
		return nil, errors.New("web: Addr is required")
	}
	if db == nil {
		return nil, errors.New("web: state.DB is required")
	}
	if handler == nil {
		return nil, errors.New("web: command handler is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 30 * 24 * time.Hour
	}
	if cfg.CookieName == "" {
		cfg.CookieName = "forge_session"
	}
	if cfg.PurgeInterval == 0 {
		cfg.PurgeInterval = time.Hour
	}

	s := &Server{
		cfg:     cfg,
		db:      db,
		handler: handler,
		logger:  logger,
	}
	s.httpServer = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

// Start begins serving HTTP requests. It blocks until ctx is cancelled or
// the server returns a non-Closed error.
func (s *Server) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("web: listen %s: %w", s.cfg.Addr, err)
	}
	s.logger.Info("web server listening", "addr", listener.Addr().String())

	if s.cfg.PurgeInterval > 0 {
		go s.purgeLoop(ctx)
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			s.logger.Warn("web server shutdown error", "error", err)
		}
	}()

	if err := s.httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("web: serve: %w", err)
	}
	return nil
}

// purgeLoop runs PurgeExpiredWebSessions on a ticker. It is started in a
// goroutine by Start and exits when ctx is cancelled.
func (s *Server) purgeLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.PurgeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.db.PurgeExpiredWebSessions()
			if err != nil {
				s.logger.Warn("web session purge failed", "error", err)
				continue
			}
			if n > 0 {
				s.logger.Info("web sessions purged", "count", n)
			}
		}
	}
}

// writeJSON encodes v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError encodes an {error: msg} JSON body with the given status.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
