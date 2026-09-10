// Package web serves a small status page showing the daemon state and the
// action log.
package web

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
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
	// Session loads a single session by id, for downloading its document.
	Session(id int64) (store.Session, error)
	// SessionActions returns one session's full action history, oldest
	// first, so its card shows everything that happened regardless of the
	// action log's own trim window.
	SessionActions(sessionID int64) ([]store.Action, error)
}

// Options configures the Server.
type Options struct {
	// Addr is the listen address, e.g. ":8080". Required for ListenAndServe.
	Addr string
	// State returns the current daemon state. Required.
	State func() session.State
	// Reader provides sessions and the action log. Required.
	Reader Reader
	// Submit queues an action triggered from the browser, with an optional
	// scan resolution override (dpi, e.g. "600"; empty uses the default).
	// When nil the page is read only.
	Submit func(action session.Action, resolution string) error
	// LemmaryURL is the base URL of the lemmary server documents were
	// uploaded to, used to link a session to its document there. Empty
	// disables the link.
	LemmaryURL string
	// Limit is how many sessions (documents) to show. Defaults to 100.
	Limit int
	// ActionsLimit is how many action log entries to show. Defaults to 50.
	ActionsLimit int
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
	if opts.ActionsLimit <= 0 {
		opts.ActionsLimit = 50
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	index, err := template.New("index.html").Funcs(templateFuncs).ParseFS(assets, "index.html")
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
	LemmaryURL     string          `json:"lemmary_url,omitempty"`
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/fragment", s.handleFragment)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/action", s.handleAction)
	mux.HandleFunc("GET /api/sessions/{id}/download", s.handleDownload)
	mux.HandleFunc("GET /api/thumbnail", s.handleThumbnail)
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

// indexData is the payload rendered by index.html. Its Blocks are rendered
// into the same markup (same classes, same per-item ids) that the polling JS
// produces from /api/fragment, so morphdom can diff the two without tearing
// the DOM down on first refresh.
type indexData struct {
	ControlEnabled bool
	Now            time.Time
	State          session.State
	Blocks         []block
	LemmaryURL     string
}

// loadIndexData gathers the data shown on the page, shared by the full page
// render and the /api/fragment poll.
func (s *Server) loadIndexData() (indexData, error) {
	sessions, err := s.opts.Reader.RecentSessions(s.opts.Limit)
	if err != nil {
		return indexData{}, err
	}

	sessionActions := make(map[int64][]store.Action, len(sessions))
	for _, sess := range sessions {
		actions, err := s.opts.Reader.SessionActions(sess.ID)
		if err != nil {
			return indexData{}, err
		}
		sessionActions[sess.ID] = actions
	}

	recentActions, err := s.opts.Reader.RecentActions(s.opts.ActionsLimit)
	if err != nil {
		return indexData{}, err
	}

	return indexData{
		ControlEnabled: s.opts.Submit != nil,
		Now:            time.Now(),
		State:          s.opts.State(),
		Blocks:         buildBlocks(sessions, sessionActions, recentActions),
		LemmaryURL:     s.opts.LemmaryURL,
	}, nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := s.loadIndexData()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.index.ExecuteTemplate(w, "index.html", data); err != nil {
		s.opts.Logger.Warn("could not render index", "error", err)
	}
}

// handleFragment renders just the dynamic "content" block, polled by the
// page's JS and patched into the DOM with morphdom.
func (s *Server) handleFragment(w http.ResponseWriter, r *http.Request) {
	data, err := s.loadIndexData()
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.index.ExecuteTemplate(w, "content", data); err != nil {
		s.opts.Logger.Warn("could not render fragment", "error", err)
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.opts.Reader.RecentSessions(s.opts.Limit)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	actions, err := s.opts.Reader.RecentActions(s.opts.ActionsLimit)
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
		LemmaryURL:     s.opts.LemmaryURL,
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

	resolution, err := resolutionFromRequest(r)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}

	// Queueing rather than running keeps the request fast: a scan takes
	// seconds and runs on the daemon's action loop.
	if err := s.opts.Submit(action, resolution); err != nil {
		s.fail(w, r, http.StatusServiceUnavailable, err)
		return
	}

	s.opts.Logger.Info("action queued from web ui", "action", string(action), "remote", r.RemoteAddr)
	writeJSON(w, http.StatusAccepted, map[string]string{"queued": string(action)})
}

// handleDownload serves a finished session's PDF as an attachment. Sessions
// are looked up by id rather than taking a path from the request, so the
// handler can only ever serve a file scantool itself produced and recorded.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, errors.New("invalid session id"))
		return
	}

	sess, err := s.opts.Reader.Session(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.fail(w, r, http.StatusNotFound, errors.New("session not found"))
		} else {
			s.fail(w, r, http.StatusInternalServerError, err)
		}
		return
	}

	if sess.OutputPath == "" {
		s.fail(w, r, http.StatusNotFound, errors.New("session has no document"))
		return
	}

	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(sess.OutputPath)+`"`)
	http.ServeFile(w, r, sess.OutputPath)
}

// handleThumbnail serves one page's thumbnail from the current session,
// ?page=N being the 1-based page number shown in the status card. There is
// nothing to show once a session finishes (State.Thumbnails is cleared along
// with the rest of its per-session state), so a stale request from a browser
// tab left open just gets a 404.
func (s *Server) handleThumbnail(w http.ResponseWriter, r *http.Request) {
	page, err := strconv.Atoi(r.FormValue("page"))
	if err != nil || page < 1 {
		s.fail(w, r, http.StatusBadRequest, errors.New("invalid page"))
		return
	}

	thumbs := s.opts.State().Thumbnails
	if page > len(thumbs) || thumbs[page-1] == "" {
		s.fail(w, r, http.StatusNotFound, errors.New("no thumbnail available"))
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, thumbs[page-1])
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
	case session.ActionScanNew, session.ActionScanPage, session.ActionFinish, session.ActionFinishNoUpload, session.ActionDiscard:
		return action, nil
	case "":
		return "", errors.New("missing action")
	default:
		return "", fmt.Errorf("unknown action %q", action)
	}
}

// resolutionFromRequest reads the optional dpi form value, e.g. set by the
// web UI's dpi dropdown before a scan. Only the values the UI offers are
// accepted; empty means the scanner's own default.
func resolutionFromRequest(r *http.Request) (string, error) {
	switch dpi := r.FormValue("dpi"); dpi {
	case "", "300", "600":
		return dpi, nil
	default:
		return "", fmt.Errorf("unsupported dpi %q", dpi)
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
