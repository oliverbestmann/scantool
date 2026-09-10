// Package session implements the scan session state machine: the rules that
// turn key presses into scanned pages and finished PDF documents.
package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oliverbestmann/scantool/internal/pdfmerge"
	"github.com/oliverbestmann/scantool/internal/scan"
	"github.com/oliverbestmann/scantool/internal/store"
)

// Action is a high level command, decoupled from the key that triggered it.
type Action string

const (
	// ActionScanNew finishes a session that already holds pages and then
	// scans the first page of a fresh session. Bound to "a".
	ActionScanNew Action = "scan-new"
	// ActionScanPage appends a page to the current session, starting one if
	// none is active. Bound to "b".
	ActionScanPage Action = "scan-page"
	// ActionFinish stores the current session as a PDF. Bound to "c".
	ActionFinish Action = "finish"
)

// Status describes what the daemon is doing right now.
type Status string

const (
	StatusIdle     Status = "idle"
	StatusScanning Status = "scanning"
	StatusSaving   Status = "saving"
	StatusError    Status = "error"
)

// BoundKeys returns the keys that trigger an action. The evdev backend uses
// it to tell a keyboard apart from the power button.
func BoundKeys() []rune { return []rune{'a', 'b', 'c'} }

// ActionForKey maps a key press to an action. Unknown keys return false.
func ActionForKey(key rune) (Action, bool) {
	switch key {
	case 'a', 'A':
		return ActionScanNew, true
	case 'b', 'B':
		return ActionScanPage, true
	case 'c', 'C':
		return ActionFinish, true
	default:
		return "", false
	}
}

// Recorder persists sessions and the action log. *store.Store implements it.
// Every method that changes more than one fact (a session plus its action
// log entry) does so atomically, so the database never observes one without
// the other.
type Recorder interface {
	CreateSessionWithAction(startedAt time.Time) (int64, error)
	AddPage(sessionID int64, page int, path string, action store.Action) error
	FinishSessionWithAction(id int64, endedAt time.Time, status, outputPath, errMsg string, action store.Action) error
	// RecordUploadWithAction stores the outcome of uploading a session's
	// document and appends the matching action, atomically.
	RecordUploadWithAction(id int64, status, errMsg string, action store.Action) error
	AppendAction(a store.Action) error
	// ActiveSession returns the most recent session still marked active, so
	// a restarted daemon can resume it instead of losing track of it. It
	// returns sql.ErrNoRows if none is active.
	ActiveSession() (store.Session, error)
	// SessionPages returns the recorded page paths of a session, so a
	// restarted daemon can rebuild its in-memory session from the database.
	SessionPages(sessionID int64) ([]string, error)
}

// Uploader posts a finished document somewhere else, e.g. a lemary server.
// *lemary.Client implements it.
type Uploader interface {
	Upload(ctx context.Context, path string) error
}

// State is a snapshot of the daemon for the web UI.
type State struct {
	Status           Status    `json:"status"`
	Since            time.Time `json:"since"`
	SessionActive    bool      `json:"session_active"`
	SessionID        int64     `json:"session_id,omitempty"`
	SessionStartedAt time.Time `json:"session_started_at,omitzero"`
	Pages            int       `json:"pages"`
	LastError        string    `json:"last_error,omitempty"`
	LastErrorAt      time.Time `json:"last_error_at,omitzero"`
	LastSavedPath    string    `json:"last_saved_path,omitempty"`
	LastSavedPages   int       `json:"last_saved_pages,omitempty"`
	LastSavedAt      time.Time `json:"last_saved_at,omitzero"`
}

// Options configures a Manager.
type Options struct {
	// OutDir receives the finished PDF documents. Required.
	OutDir string
	// WorkDir holds the per session scratch directories. Defaults to
	// OutDir/.scantool-work, which keeps pages on the same filesystem.
	WorkDir string
	// FileLayout is the time layout used for the output file name.
	// Defaults to "20060102-150405".
	FileLayout string
	// Scanner scans a single page. Required.
	Scanner scan.Scanner
	// Merger merges the pages of a session. Required.
	Merger pdfmerge.Merger
	// Recorder persists sessions and the action log. Optional.
	Recorder Recorder
	// Uploader posts a finished document to an external service. Optional;
	// when set, every saved document is uploaded and the outcome recorded
	// on the session.
	Uploader Uploader
	// Logger receives diagnostics for the service log. Defaults to
	// slog.Default().
	Logger *slog.Logger
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
}

// Manager owns the active session. Its methods are safe for concurrent use;
// actions are executed one after another.
type Manager struct {
	opts Options

	// runMu serialises actions, so a key press during a scan waits.
	runMu sync.Mutex

	mu    sync.Mutex
	state State
	cur   *activeSession
}

type activeSession struct {
	id        int64
	startedAt time.Time
	dir       string
	pages     []string
}

// New creates a Manager and prepares the output and work directories.
func New(opts Options) (*Manager, error) {
	switch {
	case opts.OutDir == "":
		return nil, fmt.Errorf("session: OutDir is required")
	case opts.Scanner == nil:
		return nil, fmt.Errorf("session: Scanner is required")
	case opts.Merger == nil:
		return nil, fmt.Errorf("session: Merger is required")
	}

	if opts.WorkDir == "" {
		opts.WorkDir = filepath.Join(opts.OutDir, ".scantool-work")
	}
	if opts.FileLayout == "" {
		opts.FileLayout = "20060102-150405"
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Recorder == nil {
		opts.Recorder = nopRecorder{}
	}

	for _, dir := range []string{opts.OutDir, opts.WorkDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("session: create %s: %w", dir, err)
		}
	}

	m := &Manager{opts: opts}
	m.state = State{Status: StatusIdle, Since: opts.Now()}

	if sess, err := opts.Recorder.ActiveSession(); err == nil {
		m.recoverSession(sess)
	} else if !errors.Is(err, sql.ErrNoRows) {
		opts.Logger.Warn("could not check for an active session to recover", "error", err)
	}

	return m, nil
}

// recoverSession rebuilds the in-memory session from the database after a
// restart, so the daemon does not depend on process memory to know what it
// was doing. If the on-disk work directory is gone, the session is recorded
// as failed instead of staying active forever.
func (m *Manager) recoverSession(sess store.Session) {
	log := m.opts.Logger.With("session", sess.ID)

	paths, err := m.opts.Recorder.SessionPages(sess.ID)
	if err != nil {
		log.Warn("could not load pages of active session", "error", err)
	}

	dir, err := m.findWorkDir(sess.ID)
	if err != nil {
		log.Warn("could not recover work directory of active session, marking it failed", "error", err)
		finishErr := m.opts.Recorder.FinishSessionWithAction(sess.ID, m.opts.Now(), store.StatusFailed, "", "work directory lost across restart",
			store.Action{Kind: store.KindSaveFailed, SessionID: sess.ID, Error: "work directory lost across restart"})
		if finishErr != nil {
			log.Warn("could not record lost session as failed", "error", finishErr)
		}
		return
	}

	m.cur = &activeSession{id: sess.ID, startedAt: sess.StartedAt, dir: dir, pages: paths}
	m.state.SessionActive = true
	m.state.SessionID = sess.ID
	m.state.SessionStartedAt = sess.StartedAt
	m.state.Pages = len(paths)

	log.Info("recovered active session", "pages", len(paths), "dir", dir)
}

// findWorkDir locates the scratch directory of a session by the id encoded
// in its name, since the directory itself is not persisted.
func (m *Manager) findWorkDir(id int64) (string, error) {
	matches, err := filepath.Glob(filepath.Join(m.opts.WorkDir, fmt.Sprintf("session-%d-*", id)))
	if err != nil {
		return "", fmt.Errorf("session: search work directory: %w", err)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("session: no work directory for session %d", id)
	}
	return matches[0], nil
}

// State returns a snapshot of the current state.
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// HandleKey performs the action bound to key. Unknown keys are ignored.
func (m *Manager) HandleKey(ctx context.Context, key rune) error {
	action, ok := ActionForKey(key)
	if !ok {
		m.opts.Logger.Debug("ignoring key", "key", string(key))
		m.record(store.Action{Kind: store.KindKeyIgnored, Detail: string(key)})
		return nil
	}

	m.opts.Logger.Info("key pressed", "key", string(key), "action", string(action))
	m.record(store.Action{
		Kind:      store.KindKeyPressed,
		Detail:    string(key),
		SessionID: m.State().SessionID,
	})
	return m.Do(ctx, action)
}

// Do performs an action.
func (m *Manager) Do(ctx context.Context, action Action) error {
	m.runMu.Lock()
	defer m.runMu.Unlock()

	switch action {
	case ActionScanNew:
		// "a" while a session already holds pages behaves like "c" followed
		// by "a": the previous document is stored first.
		if m.hasPages() {
			if err := m.finish(ctx); err != nil {
				return err
			}
		}
		return m.scan(ctx)

	case ActionScanPage:
		// "b" without a session simply starts one.
		return m.scan(ctx)

	case ActionFinish:
		return m.finish(ctx)

	default:
		return fmt.Errorf("session: unknown action %q", action)
	}
}

// Close stores a still open session, so shutting the daemon down does not
// lose pages that were already scanned.
func (m *Manager) Close() error {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	return m.finish(context.Background())
}

func (m *Manager) hasPages() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur != nil && len(m.cur.pages) > 0
}

// scan appends one page to the current session, starting one if needed.
// Must be called with runMu held.
func (m *Manager) scan(ctx context.Context) error {
	if m.cur == nil {
		if err := m.start(); err != nil {
			m.fail(err)
			return err
		}
	}

	cur := m.cur
	page := len(cur.pages) + 1
	dest := filepath.Join(cur.dir, fmt.Sprintf("page-%03d.pdf", page))

	log := m.opts.Logger.With("session", cur.id, "page", page)
	log.Info("scanning page")

	m.setStatus(StatusScanning)

	if err := m.opts.Scanner.ScanPage(ctx, scan.Request{Dest: dest, SessionID: cur.id, Page: page}); err != nil {
		// The session stays open on purpose: pressing "b" retries the page
		// without losing the pages scanned so far.
		log.Error("scan failed", "error", err)
		m.record(store.Action{
			Kind:      store.KindScanFailed,
			SessionID: cur.id,
			Page:      page,
			Error:     err.Error(),
		})
		m.fail(err)
		return err
	}

	// The page is on disk; recording it and its action atomically keeps the
	// database's page count and action log from ever disagreeing with each
	// other, even though a recording failure itself must not lose the page
	// that was already scanned.
	if err := m.opts.Recorder.AddPage(cur.id, page, dest, store.Action{Kind: store.KindPageScanned, SessionID: cur.id, Page: page}); err != nil {
		log.Warn("could not record scanned page", "error", err)
	}

	m.mu.Lock()
	cur.pages = append(cur.pages, dest)
	pages := len(cur.pages)
	m.state.Pages = pages
	m.state.Status = StatusIdle
	m.state.Since = m.opts.Now()
	m.state.LastError = ""
	m.mu.Unlock()

	log.Info("page scanned", "pages", pages)
	return nil
}

// start opens a new session. Must be called with runMu held.
func (m *Manager) start() error {
	now := m.opts.Now()

	id, err := m.opts.Recorder.CreateSessionWithAction(now)
	if err != nil {
		return fmt.Errorf("session: create: %w", err)
	}

	dir, err := os.MkdirTemp(m.opts.WorkDir, fmt.Sprintf("session-%d-", id))
	if err != nil {
		return fmt.Errorf("session: create work directory: %w", err)
	}

	m.mu.Lock()
	m.cur = &activeSession{id: id, startedAt: now, dir: dir}
	m.state.SessionActive = true
	m.state.SessionID = id
	m.state.SessionStartedAt = now
	m.state.Pages = 0
	m.mu.Unlock()

	m.opts.Logger.Info("session started", "session", id, "dir", dir)
	return nil
}

// finish stores the current session. Must be called with runMu held.
func (m *Manager) finish(ctx context.Context) error {
	cur := m.cur
	if cur == nil {
		m.opts.Logger.Info("no active session to finish")
		return nil
	}

	log := m.opts.Logger.With("session", cur.id)
	now := m.opts.Now()

	// An empty session has nothing worth storing, e.g. "c" right after a
	// failed first scan.
	if len(cur.pages) == 0 {
		log.Info("session discarded, no pages scanned")
		err := m.opts.Recorder.FinishSessionWithAction(cur.id, now, store.StatusDiscarded, "", "",
			store.Action{Kind: store.KindSessionDiscarded, SessionID: cur.id})
		if err != nil {
			log.Warn("could not record discarded session", "error", err)
		}
		m.removeWork(cur, log)
		m.clear(StatusIdle)
		return nil
	}

	m.setStatus(StatusSaving)

	dest, err := m.outputPath(cur.startedAt)
	if err == nil {
		err = m.opts.Merger.Merge(cur.pages, dest)
	}
	if err != nil {
		// The scratch directory is kept so the pages can be recovered by hand.
		log.Error("could not store session", "error", err, "pages_dir", cur.dir)
		recErr := m.opts.Recorder.FinishSessionWithAction(cur.id, now, store.StatusFailed, "", err.Error(),
			store.Action{
				Kind:      store.KindSaveFailed,
				SessionID: cur.id,
				Page:      len(cur.pages),
				Detail:    cur.dir,
				Error:     err.Error(),
			})
		if recErr != nil {
			log.Warn("could not record failed session", "error", recErr)
		}
		m.clear(StatusError)
		m.fail(err)
		return err
	}

	pages := len(cur.pages)
	if err := m.opts.Recorder.FinishSessionWithAction(cur.id, now, store.StatusSaved, dest, "",
		store.Action{Kind: store.KindSessionSaved, SessionID: cur.id, Page: pages, Detail: dest}); err != nil {
		log.Warn("could not record finished session", "error", err)
	}

	if m.opts.Uploader != nil {
		m.upload(ctx, cur.id, dest, log)
	}

	m.removeWork(cur, log)

	m.mu.Lock()
	m.cur = nil
	m.state.SessionActive = false
	m.state.SessionID = 0
	m.state.SessionStartedAt = time.Time{}
	m.state.Pages = 0
	m.state.Status = StatusIdle
	m.state.Since = now
	m.state.LastError = ""
	m.state.LastSavedPath = dest
	m.state.LastSavedPages = pages
	m.state.LastSavedAt = now
	m.mu.Unlock()

	log.Info("session stored", "path", dest, "pages", pages)
	return nil
}

// Record adds an entry to the action log. Used by the daemon for events that
// are not part of the state machine, such as startup and shutdown.
func (m *Manager) Record(a store.Action) { m.record(a) }

func (m *Manager) record(a store.Action) {
	if a.Time.IsZero() {
		a.Time = m.opts.Now()
	}
	if err := m.opts.Recorder.AppendAction(a); err != nil {
		m.opts.Logger.Warn("could not append to action log", "error", err, "kind", string(a.Kind))
	}
}

// upload posts the finished document and records the outcome on the
// session. The document stays on disk either way, so a failed upload can be
// retried later.
func (m *Manager) upload(ctx context.Context, sessionID int64, path string, log *slog.Logger) {
	status, errMsg := store.UploadUploaded, ""
	kind := store.KindUploadSucceeded

	if err := m.opts.Uploader.Upload(ctx, path); err != nil {
		log.Error("could not upload document", "error", err, "path", path)
		status, errMsg = store.UploadFailed, err.Error()
		kind = store.KindUploadFailed
	} else {
		log.Info("document uploaded", "path", path)
	}

	action := store.Action{Kind: kind, SessionID: sessionID, Detail: path, Error: errMsg}
	if err := m.opts.Recorder.RecordUploadWithAction(sessionID, status, errMsg, action); err != nil {
		log.Warn("could not record upload outcome", "error", err)
	}
}

func (m *Manager) removeWork(cur *activeSession, log *slog.Logger) {
	if err := os.RemoveAll(cur.dir); err != nil {
		log.Warn("could not remove work directory", "dir", cur.dir, "error", err)
	}
}

// outputPath builds outDir/<timestamp>.pdf, adding a counter on collision.
func (m *Manager) outputPath(startedAt time.Time) (string, error) {
	base := startedAt.Format(m.opts.FileLayout)

	for i := 1; i < 1000; i++ {
		name := base + ".pdf"
		if i > 1 {
			name = fmt.Sprintf("%s-%d.pdf", base, i)
		}

		path := filepath.Join(m.opts.OutDir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return path, nil
		} else if err != nil {
			return "", fmt.Errorf("session: check output path: %w", err)
		}
	}

	return "", fmt.Errorf("session: no free output name for %s", base)
}

func (m *Manager) setStatus(s Status) {
	now := m.opts.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Status = s
	m.state.Since = now
}

func (m *Manager) fail(err error) {
	now := m.opts.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.Status = StatusError
	m.state.Since = now
	m.state.LastError = err.Error()
	m.state.LastErrorAt = now
}

// clear drops the active session from the state.
func (m *Manager) clear(status Status) {
	now := m.opts.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cur = nil
	m.state.SessionActive = false
	m.state.SessionID = 0
	m.state.SessionStartedAt = time.Time{}
	m.state.Pages = 0
	m.state.Status = status
	m.state.Since = now
}

type nopRecorder struct{}

func (nopRecorder) CreateSessionWithAction(time.Time) (int64, error) { return 0, nil }
func (nopRecorder) AddPage(int64, int, string, store.Action) error   { return nil }
func (nopRecorder) AppendAction(store.Action) error                  { return nil }
func (nopRecorder) SessionPages(int64) ([]string, error)             { return nil, nil }
func (nopRecorder) ActiveSession() (store.Session, error)            { return store.Session{}, sql.ErrNoRows }

func (nopRecorder) FinishSessionWithAction(int64, time.Time, string, string, string, store.Action) error {
	return nil
}

func (nopRecorder) RecordUploadWithAction(int64, string, string, store.Action) error { return nil }
