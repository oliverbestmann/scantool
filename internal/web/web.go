// Package web serves a small status page showing the daemon state and the
// action log.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/oliverbestmann/scantool/internal/session"
	"github.com/oliverbestmann/scantool/internal/store"
)

//go:embed index.html
var assets embed.FS

// Reader provides the history shown on the page.
type Reader interface {
	RecentSessions(limit int) ([]store.Session, error)
	RecentActions(limit int) ([]store.Action, error)
}

// Options configures the Server.
type Options struct {
	// Addr is the listen address, e.g. ":8080". Required for ListenAndServe.
	Addr string
	// State returns the current daemon state. Required.
	State func() session.State
	// Reader provides sessions and the action log. Required.
	Reader Reader
	// Submit queues an action triggered from the browser. When nil the page
	// is read only.
	Submit func(session.Action) error
	// Limit is how many log entries and sessions to show. Defaults to 100.
	Limit int
	// Logger receives diagnostics. Defaults to slog.Default().
	Logger *slog.Logger
}

// Server serves the status page.
type Server struct {
	opts  Options
	index *template.Template
}

// New creates a Server.
func New(opts Options) (*Server, error) {
	switch {
	case opts.State == nil:
		return nil, errors.New("web: State is required")
	case opts.Reader == nil:
		return nil, errors.New("web: Reader is required")
	}
	if opts.Limit <= 0 {
		opts.Limit = 100
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	index, err := template.ParseFS(assets, "index.html")
	if err != nil {
		return nil, fmt.Errorf("web: parse template: %w", err)
	}

	return &Server{opts: opts, index: index}, nil
}

// snapshot is the payload of /api/state.
type snapshot struct {
	Now            time.Time       `json:"now"`
	State          session.State   `json:"state"`
	Sessions       []store.Session `json:"sessions"`
	Actions        []store.Action  `json:"actions"`
	ControlEnabled bool            `json:"control_enabled"`
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/action", s.handleAction)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})
	return mux
}

// ListenAndServe serves until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	stop := context.AfterFunc(ctx, func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	})
	defer stop()

	s.opts.Logger.Info("web ui listening", "addr", s.opts.Addr)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("web: serve: %w", err)
	}
	return nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	data := map[string]any{
		"ControlEnabled": s.opts.Submit != nil,
	}
	if err := s.index.Execute(w, data); err != nil {
		s.opts.Logger.Warn("could not render index", "error", err)
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.opts.Reader.RecentSessions(s.opts.Limit)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	actions, err := s.opts.Reader.RecentActions(s.opts.Limit)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, snapshot{
		Now:            time.Now(),
		State:          s.opts.State(),
		Sessions:       sessions,
		Actions:        actions,
		ControlEnabled: s.opts.Submit != nil,
	})
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	if s.opts.Submit == nil {
		s.fail(w, r, http.StatusForbidden, errors.New("web control is disabled"))
		return
	}

	action, err := actionFromRequest(r)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}

	// Queueing rather than running keeps the request fast: a scan takes
	// seconds and runs on the daemon's action loop.
	if err := s.opts.Submit(action); err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, err)
		return
	}

	s.opts.Logger.Info("action queued from web ui", "action", string(action), "remote", r.RemoteAddr)
	writeJSON(w, http.StatusAccepted, map[string]string{"queued": string(action)})
}

// actionFromRequest accepts either ?action=scan-page or ?key=b.
func actionFromRequest(r *http.Request) (session.Action, error) {
	if key := r.FormValue("key"); key != "" {
		runes := []rune(key)
		if len(runes) != 1 {
			return "", fmt.Errorf("key %q is not a single key", key)
		}
		action, ok := session.ActionForKey(runes[0])
		if !ok {
			return "", fmt.Errorf("key %q is not bound to an action", key)
		}
		return action, nil
	}

	switch action := session.Action(r.FormValue("action")); action {
	case session.ActionScanNew, session.ActionScanPage, session.ActionFinish:
		return action, nil
	case "":
		return "", errors.New("missing action")
	default:
		return "", fmt.Errorf("unknown action %q", action)
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, code int, err error) {
	s.opts.Logger.Warn("request failed", "path", r.URL.Path, "status", code, "error", err)
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(payload)
}
