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
	"strings"
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
	// ActionFinishNoUpload stores the current session as a PDF like
	// ActionFinish, but skips the uploader. Not bound to a key; only
	// reachable from the web UI.
	ActionFinishNoUpload Action = "finish-no-upload"
	// ActionDiscard drops the current session without storing it, whether or
	// not it holds pages. Not bound to a key; only reachable from the web UI.
	ActionDiscard Action = "discard"
)

// resolutionCtxKey carries a per-request scan resolution override through
// Do/DoLogged, e.g. the dpi chosen in the web UI. Key presses never set it,
// so they keep scanning at the scanner's configured default.
type resolutionCtxKey struct{}

// WithResolution attaches a scan resolution override (in dpi, e.g. "600")
// to ctx, applied to the next scan action performed with it. Empty uses the
// scanner's own default.
func WithResolution(ctx context.Context, dpi string) context.Context {
	return context.WithValue(ctx, resolutionCtxKey{}, dpi)
}

func resolutionFromContext(ctx context.Context) string {
	dpi, _ := ctx.Value(resolutionCtxKey{}).(string)
	return dpi
}

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
	RecordUploadWithAction(id int64, status, lemmaryID, errMsg string, action store.Action) error
	// AppendAction stores one action log entry and returns its id.
	AppendAction(a store.Action) (int64, error)
	// SetActionSessionID attaches a session to an action log entry recorded
	// before the session existed, e.g. the key press that started it.
	SetActionSessionID(actionID, sessionID int64) error
	// ActiveSession returns the most recent session still marked active, so
	// a restarted daemon can resume it instead of losing track of it. It
	// returns sql.ErrNoRows if none is active.
	ActiveSession() (store.Session, error)
	// SessionPages returns the recorded page paths of a session, so a
	// restarted daemon can rebuild its in-memory session from the database.
	SessionPages(sessionID int64) ([]string, error)
}

// Uploader posts a finished document somewhere else, e.g. a lemmary server.
// *lemmary.Client implements it. It returns the id of the record created on
// the remote server.
type Uploader interface {
	Upload(ctx context.Context, path string) (string, error)
}

// State is a snapshot of the daemon for the web UI.
type State struct {
	Status           Status    `json:"status"`
	Since            time.Time `json:"since"`
	SessionActive    bool      `json:"session_active"`
	SessionID        int64     `json:"session_id,omitempty"`
	SessionStartedAt time.Time `json:"session_started_at,omitzero"`
	Pages            int       `json:"pages"`
	PageSizesKB      []int64   `json:"page_sizes_kb,omitempty"`
	// Thumbnails are the on-disk paths of the current session's page
	// thumbnails, in scan order, served via /api/thumbnail?page=N. Not
	// exposed in the JSON state: they're local filesystem paths, not
	// something a client can use directly.
	Thumbnails     []string  `json:"-"`
	LastError      string    `json:"last_error,omitempty"`
	LastErrorAt    time.Time `json:"last_error_at,omitzero"`
	LastSavedPath  string    `json:"last_saved_path,omitempty"`
	LastSavedPages int       `json:"last_saved_pages,omitempty"`
	LastSavedAt    time.Time `json:"last_saved_at,omitzero"`
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

	// pending tracks background ProcessImage calls dispatched by a
	// TwoPhaseScanner, so finish/discard can wait for them before touching
	// the work directory or merging pages. Add and Wait are only ever
	// called while runMu is held, so they never race with each other.
	pending sync.WaitGroup
	// abortErr is set by a failed background ProcessImage call. Once set,
	// the session can no longer be saved; guarded by Manager.mu since a
	// processing goroutine sets it outside of runMu.
	abortErr error
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
	m.state.PageSizesKB = pageSizesKB(paths)
	m.state.Thumbnails = thumbnailPaths(paths)

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
		m.record(store.Action{Kind: store.KindKeyIgnored, Detail: string(key), SessionID: m.State().SessionID})
		return nil
	}

	m.opts.Logger.Info("key pressed", "key", string(key), "action", string(action))
	return m.DoLogged(ctx, action, store.KindKeyPressed, string(key))
}

// DoLogged performs action like Do, additionally recording kind/detail as
// the action log entry for whatever triggered it (a key press, a web
// request). The entry is attached to the session active beforehand, or, if
// none was active yet (the action starts one, e.g. scanning the first page
// from idle), to the session Do just created — so the triggering event
// always ends up inside the session it affected instead of floating as a
// session-less entry.
func (m *Manager) DoLogged(ctx context.Context, action Action, kind store.Kind, detail string) error {
	preSessionID := m.State().SessionID
	actionID := m.record(store.Action{Kind: kind, Detail: detail, SessionID: preSessionID})

	err := m.Do(ctx, action)

	if preSessionID == 0 {
		if sessionID := m.State().SessionID; sessionID != 0 {
			if patchErr := m.opts.Recorder.SetActionSessionID(actionID, sessionID); patchErr != nil {
				m.opts.Logger.Warn("could not attach session to action", "error", patchErr)
			}
		}
	}

	return err
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
			if err := m.finish(ctx, true); err != nil {
				return err
			}
		}
		return m.scan(ctx)

	case ActionScanPage:
		// "b" without a session simply starts one.
		return m.scan(ctx)

	case ActionFinish:
		return m.finish(ctx, true)

	case ActionFinishNoUpload:
		return m.finish(ctx, false)

	case ActionDiscard:
		return m.discard()

	default:
		return fmt.Errorf("session: unknown action %q", action)
	}
}

// Close stores a still open session, so shutting the daemon down does not
// lose pages that were already scanned.
func (m *Manager) Close() error {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	return m.finish(context.Background(), true)
}

func (m *Manager) hasPages() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur != nil && len(m.cur.pages) > 0
}

// scan appends one page to the current session, starting one if needed. If
// the scanner can separate acquiring the image from turning it into a PDF
// (scan.TwoPhaseScanner), the PDF is produced by a background goroutine so
// the next page can be acquired right away instead of waiting for it. Must
// be called with runMu held.
func (m *Manager) scan(ctx context.Context) error {
	if m.cur == nil {
		if err := m.start(); err != nil {
			m.fail(err)
			return err
		}
	}

	cur := m.cur

	m.mu.Lock()
	abortErr := cur.abortErr
	m.mu.Unlock()
	if abortErr != nil {
		// A page failed to process in the background. The session cannot
		// produce a valid document anymore; "c" or a discard clears it.
		return abortErr
	}

	page := len(cur.pages) + 1
	dest := filepath.Join(cur.dir, fmt.Sprintf("page-%03d.pdf", page))
	thumbDest := thumbPath(dest)
	req := scan.Request{Dest: dest, SessionID: cur.id, Page: page, Resolution: resolutionFromContext(ctx), ThumbDest: thumbDest}

	log := m.opts.Logger.With("session", cur.id, "page", page)
	log.Info("scanning page")

	m.setStatus(StatusScanning)

	started := m.opts.Now()

	if two, ok := m.opts.Scanner.(scan.TwoPhaseScanner); ok {
		image, err := two.AcquireImage(ctx, req)
		if err != nil {
			return m.scanFailed(cur, page, err, log)
		}

		cur.pending.Add(1)
		go m.processPage(two, cur, req, image, log)
	} else if err := m.opts.Scanner.ScanPage(ctx, req); err != nil {
		return m.scanFailed(cur, page, err, log)
	}

	// The page is on disk, or (for a TwoPhaseScanner) being written by the
	// background goroutine just started; recording it and its action
	// atomically keeps the database's page count and action log from ever
	// disagreeing with each other, even though a recording failure itself
	// must not lose the page that was already scanned. The size in the
	// detail is best effort: for a TwoPhaseScanner the file usually is not
	// there yet, so it is simply omitted.
	detail := pageScanDetail(dest, m.opts.Now().Sub(started))
	if err := m.opts.Recorder.AddPage(cur.id, page, dest, store.Action{Kind: store.KindPageScanned, SessionID: cur.id, Page: page, Detail: detail}); err != nil {
		log.Warn("could not record scanned page", "error", err)
	}

	m.mu.Lock()
	cur.pages = append(cur.pages, dest)
	pages := len(cur.pages)
	m.state.Pages = pages
	m.state.PageSizesKB = append(m.state.PageSizesKB, 0)
	m.state.Thumbnails = append(m.state.Thumbnails, "")
	m.state.Status = StatusIdle
	m.state.Since = m.opts.Now()
	m.state.LastError = ""
	m.mu.Unlock()

	// For a synchronous Scanner, dest and thumbDest already exist by now.
	// For a TwoPhaseScanner they usually don't yet; processPage calls this
	// again once the background conversion finishes.
	m.updatePageResult(cur.id, page, dest, thumbDest)

	log.Info("page scanned", "pages", pages)
	return nil
}

// updatePageResult refreshes a page's size and thumbnail in the state once
// its PDF (and thumbnail) exist on disk. Called right after a synchronous
// scan, and again by processPage when a TwoPhaseScanner's background
// conversion completes. A page whose file is not there yet is simply left
// as is; it is safe to call more than once for the same page.
func (m *Manager) updatePageResult(sessionID int64, page int, dest, thumbDest string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cur == nil || m.cur.id != sessionID {
		return
	}

	idx := page - 1
	if idx < 0 || idx >= len(m.state.PageSizesKB) {
		return
	}

	if size := pageSizeKB(dest); size > 0 {
		m.state.PageSizesKB[idx] = size
	}
	if _, err := os.Stat(thumbDest); err == nil {
		m.state.Thumbnails[idx] = thumbDest
	}
}

// pageSizeKB is the size of the page file at path, rounded up to the next
// KB. A stat failure best-effort returns 0 rather than failing whatever
// operation wanted the size.
func pageSizeKB(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return (info.Size() + 1023) / 1024
}

// pageSizesKB is pageSizeKB applied to every path, in order.
func pageSizesKB(paths []string) []int64 {
	sizes := make([]int64, len(paths))
	for i, p := range paths {
		sizes[i] = pageSizeKB(p)
	}
	return sizes
}

// thumbPath derives a page's thumbnail path from its PDF path, e.g.
// "page-003.pdf" -> "page-003-thumb.jpg".
func thumbPath(pagePath string) string {
	return strings.TrimSuffix(pagePath, filepath.Ext(pagePath)) + "-thumb.jpg"
}

// thumbnailPaths derives each page's thumbnail path from paths, index
// aligned so page N is Thumbnails[N-1] for /api/thumbnail?page=N; a missing
// thumbnail (stat failure) leaves that slot empty. Used to rebuild
// State.Thumbnails after a restart.
func thumbnailPaths(paths []string) []string {
	thumbs := make([]string, len(paths))
	for i, p := range paths {
		thumb := thumbPath(p)
		if _, err := os.Stat(thumb); err == nil {
			thumbs[i] = thumb
		}
	}
	return thumbs
}

// pageScanDetail renders the action log detail line for a scanned page,
// e.g. "142 KB, 1.8s". The file size is best effort: a stat failure just
// drops it rather than failing the scan that already succeeded.
func pageScanDetail(path string, elapsed time.Duration) string {
	detail := fmt.Sprintf("%.1fs", elapsed.Seconds())
	if size := pageSizeKB(path); size > 0 {
		detail = fmt.Sprintf("%d KB, %s", size, detail)
	}
	return detail
}

// scanFailed records a failed acquisition of a page. The session stays open
// on purpose: pressing "b" retries the page without losing the pages
// scanned so far. Must be called with runMu held.
func (m *Manager) scanFailed(cur *activeSession, page int, err error, log *slog.Logger) error {
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

// processPage is a TwoPhaseScanner's slow half, run in the background by
// scan so the next page can be acquired while this one converts. A failure
// aborts the session (see activeSession.abortErr): by the time it happens,
// later pages may already be queued behind this one, so there is no sound
// way to drop just this page and keep going. It uses a context detached
// from the one scan() was called with, since that call has already
// returned by the time this runs.
func (m *Manager) processPage(two scan.TwoPhaseScanner, cur *activeSession, req scan.Request, image []byte, log *slog.Logger) {
	defer cur.pending.Done()

	if err := two.ProcessImage(context.Background(), req, image); err != nil {
		log.Error("processing page failed", "error", err)

		m.mu.Lock()
		if cur.abortErr == nil {
			cur.abortErr = err
		}
		m.mu.Unlock()

		m.record(store.Action{
			Kind:      store.KindProcessFailed,
			SessionID: cur.id,
			Page:      req.Page,
			Error:     err.Error(),
		})
		m.fail(err)
		return
	}

	m.updatePageResult(cur.id, req.Page, req.Dest, req.ThumbDest)
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
	m.state.PageSizesKB = nil
	m.state.Thumbnails = nil
	m.mu.Unlock()

	m.opts.Logger.Info("session started", "session", id, "dir", dir)
	return nil
}

// finish stores the current session, uploading it when upload is true and an
// Uploader is configured. Must be called with runMu held.
func (m *Manager) finish(ctx context.Context, upload bool) error {
	cur := m.cur
	if cur == nil {
		m.opts.Logger.Info("no active session to finish")
		return nil
	}

	// A page recorded via AddPage does not really exist on disk until its
	// background processing goroutine (if any) finishes; wait for them all
	// before deciding whether there is anything to merge.
	cur.pending.Wait()

	log := m.opts.Logger.With("session", cur.id)

	m.mu.Lock()
	abortErr := cur.abortErr
	m.mu.Unlock()

	if abortErr != nil {
		// The scratch directory is kept so the pages can be recovered by hand.
		log.Error("could not store session: a page failed to process", "error", abortErr, "pages_dir", cur.dir)
		recErr := m.opts.Recorder.FinishSessionWithAction(cur.id, m.opts.Now(), store.StatusFailed, "", abortErr.Error(),
			store.Action{
				Kind:      store.KindSaveFailed,
				SessionID: cur.id,
				Page:      len(cur.pages),
				Detail:    cur.dir,
				Error:     abortErr.Error(),
			})
		if recErr != nil {
			log.Warn("could not record failed session", "error", recErr)
		}
		m.clear(StatusError)
		m.fail(abortErr)
		return abortErr
	}

	// An empty session has nothing worth storing, e.g. "c" right after a
	// failed first scan.
	if len(cur.pages) == 0 {
		return m.discard()
	}

	now := m.opts.Now()

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

	if upload && m.opts.Uploader != nil {
		m.upload(ctx, cur.id, dest, log)
	}

	m.removeWork(cur, log)

	m.mu.Lock()
	m.cur = nil
	m.state.SessionActive = false
	m.state.SessionID = 0
	m.state.SessionStartedAt = time.Time{}
	m.state.Pages = 0
	m.state.PageSizesKB = nil
	m.state.Thumbnails = nil
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

// discard drops the current session without storing it, whether or not it
// holds pages. Must be called with runMu held.
func (m *Manager) discard() error {
	cur := m.cur
	if cur == nil {
		m.opts.Logger.Info("no active session to discard")
		return nil
	}

	// Let any background page processing finish (or fail) before removing
	// the work directory it reads and writes.
	cur.pending.Wait()

	log := m.opts.Logger.With("session", cur.id)
	now := m.opts.Now()

	if err := m.opts.Recorder.FinishSessionWithAction(cur.id, now, store.StatusDiscarded, "", "",
		store.Action{Kind: store.KindSessionDiscarded, SessionID: cur.id, Page: len(cur.pages)}); err != nil {
		log.Warn("could not record discarded session", "error", err)
	}

	m.removeWork(cur, log)
	m.clear(StatusIdle)

	log.Info("session discarded", "pages", len(cur.pages))
	return nil
}

// Record adds an entry to the action log. Used by the daemon for events that
// are not part of the state machine, such as startup and shutdown.
func (m *Manager) Record(a store.Action) { m.record(a) }

// record appends a to the action log and returns its id, so a caller that
// logged an event before its session existed can attach the session once
// known, via DoLogged.
func (m *Manager) record(a store.Action) int64 {
	if a.Time.IsZero() {
		a.Time = m.opts.Now()
	}
	id, err := m.opts.Recorder.AppendAction(a)
	if err != nil {
		m.opts.Logger.Warn("could not append to action log", "error", err, "kind", string(a.Kind))
	}
	return id
}

// upload posts the finished document and records the outcome on the
// session. The document stays on disk either way, so a failed upload can be
// retried later.
func (m *Manager) upload(ctx context.Context, sessionID int64, path string, log *slog.Logger) {
	status, errMsg := store.UploadUploaded, ""
	kind := store.KindUploadSucceeded

	lemmaryID, err := m.opts.Uploader.Upload(ctx, path)
	detail := path
	if err != nil {
		log.Error("could not upload document", "error", err, "path", path)
		status, errMsg = store.UploadFailed, err.Error()
		kind = store.KindUploadFailed
	} else {
		log.Info("document uploaded", "path", path, "lemmary_id", lemmaryID)
		detail = lemmaryID
	}

	action := store.Action{Kind: kind, SessionID: sessionID, Detail: detail, Error: errMsg}
	if err := m.opts.Recorder.RecordUploadWithAction(sessionID, status, lemmaryID, errMsg, action); err != nil {
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
	m.state.PageSizesKB = nil
	m.state.Thumbnails = nil
	m.state.Status = status
	m.state.Since = now
}

type nopRecorder struct{}

func (nopRecorder) CreateSessionWithAction(time.Time) (int64, error) { return 0, nil }
func (nopRecorder) AddPage(int64, int, string, store.Action) error   { return nil }
func (nopRecorder) AppendAction(store.Action) (int64, error)         { return 0, nil }
func (nopRecorder) SetActionSessionID(int64, int64) error            { return nil }
func (nopRecorder) SessionPages(int64) ([]string, error)             { return nil, nil }
func (nopRecorder) ActiveSession() (store.Session, error)            { return store.Session{}, sql.ErrNoRows }

func (nopRecorder) FinishSessionWithAction(int64, time.Time, string, string, string, store.Action) error {
	return nil
}

func (nopRecorder) RecordUploadWithAction(int64, string, string, string, store.Action) error {
	return nil
}
