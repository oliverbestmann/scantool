package session_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oliverbestmann/scantool/internal/pdfmerge"
	"github.com/oliverbestmann/scantool/internal/scan"
	"github.com/oliverbestmann/scantool/internal/session"
	"github.com/oliverbestmann/scantool/internal/store"
	"github.com/oliverbestmann/scantool/internal/testpdf"
)

// fakeScanner stands in for scan-page.sh: it writes a recognisable text file
// instead of talking to SANE, and can be told to fail on a given attempt.
type fakeScanner struct {
	mu       sync.Mutex
	requests []scan.Request

	// errs maps the 1-based scan attempt to the error it should return.
	errs map[int]error
	// hook runs before each scan, e.g. to inspect the daemon state.
	hook func(scan.Request)
}

func (f *fakeScanner) ScanPage(_ context.Context, req scan.Request) error {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	attempt := len(f.requests)
	err := f.errs[attempt]
	hook := f.hook
	f.mu.Unlock()

	if hook != nil {
		hook(req)
	}
	if err != nil {
		return err
	}
	return os.WriteFile(req.Dest, []byte(fmt.Sprintf("session %d page %d\n", req.SessionID, req.Page)), 0o644)
}

func (f *fakeScanner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// fakePipelinedScanner is a scan.TwoPhaseScanner test double. AcquireImage
// always returns right away; ProcessImage optionally blocks on a per-page
// gate (so a test can hold page N "processing" while it acquires page N+1)
// and optionally fails for a given page (so tests can exercise the abort
// path). Every ProcessImage call, successful or not, sends its request on
// done so tests can wait for it deterministically instead of sleeping.
type fakePipelinedScanner struct {
	mu          sync.Mutex
	acquired    []scan.Request
	processed   []scan.Request
	processErrs map[int]error
	gate        map[int]chan struct{}
	done        chan scan.Request
}

func newFakePipelinedScanner() *fakePipelinedScanner {
	return &fakePipelinedScanner{
		processErrs: map[int]error{},
		gate:        map[int]chan struct{}{},
		done:        make(chan scan.Request, 16),
	}
}

func (f *fakePipelinedScanner) ScanPage(ctx context.Context, req scan.Request) error {
	image, err := f.AcquireImage(ctx, req)
	if err != nil {
		return err
	}
	return f.ProcessImage(ctx, req, image)
}

func (f *fakePipelinedScanner) AcquireImage(_ context.Context, req scan.Request) ([]byte, error) {
	f.mu.Lock()
	f.acquired = append(f.acquired, req)
	f.mu.Unlock()
	return []byte(fmt.Sprintf("session %d page %d\n", req.SessionID, req.Page)), nil
}

func (f *fakePipelinedScanner) ProcessImage(_ context.Context, req scan.Request, image []byte) error {
	f.mu.Lock()
	gate := f.gate[req.Page]
	err := f.processErrs[req.Page]
	f.mu.Unlock()

	if gate != nil {
		<-gate
	}

	f.mu.Lock()
	f.processed = append(f.processed, req)
	f.mu.Unlock()
	defer func() { f.done <- req }()

	if err != nil {
		return err
	}
	return os.WriteFile(req.Dest, image, 0o644)
}

func (f *fakePipelinedScanner) acquiredCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.acquired)
}

func (f *fakePipelinedScanner) hasProcessed(page int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.processed {
		if req.Page == page {
			return true
		}
	}
	return false
}

// fakeUploader stands in for the lemmary client.
type fakeUploader struct {
	mu    sync.Mutex
	paths []string
	id    string
	err   error
}

func (f *fakeUploader) Upload(_ context.Context, path string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, path)
	if f.err != nil {
		return "", f.err
	}
	return f.id, nil
}

func (f *fakeUploader) uploaded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// fakeMerger concatenates the page files, so tests can assert both the number
// of pages and their order.
type fakeMerger struct {
	mu    sync.Mutex
	calls [][]string
	err   error
}

func (f *fakeMerger) Merge(pages []string, dest string) error {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), pages...))
	err := f.err
	f.mu.Unlock()

	if err != nil {
		return err
	}

	var merged []byte
	for _, page := range pages {
		content, readErr := os.ReadFile(page)
		if readErr != nil {
			return readErr
		}
		merged = append(merged, content...)
	}
	return os.WriteFile(dest, merged, 0o644)
}

type harness struct {
	t       *testing.T
	outDir  string
	workDir string
	db      *store.Store
	manager *session.Manager
	scanner *fakeScanner
	merger  *fakeMerger

	mu  sync.Mutex
	now time.Time
}

func newHarness(t *testing.T, tweak func(*session.Options)) *harness {
	t.Helper()

	root := t.TempDir()
	h := &harness{
		t:       t,
		outDir:  filepath.Join(root, "scans"),
		workDir: filepath.Join(root, "work"),
		scanner: &fakeScanner{errs: map[int]error{}},
		merger:  &fakeMerger{},
		now:     time.Date(2026, 9, 8, 11, 44, 0, 0, time.UTC),
	}

	db, err := store.Open(filepath.Join(root, "scantool.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	h.db = db

	opts := session.Options{
		OutDir:   h.outDir,
		WorkDir:  h.workDir,
		Scanner:  h.scanner,
		Merger:   h.merger,
		Recorder: db,
		Now:      h.clock,
	}
	if tweak != nil {
		tweak(&opts)
	}

	h.manager, err = session.New(opts)
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	return h
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

// advance moves the fake clock, so two documents do not collide by accident.
func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

// press feeds keys to the manager, ignoring the errors that the manager
// already reports through its state and the action log.
func (h *harness) press(keys ...rune) {
	h.t.Helper()

	for _, key := range keys {
		h.manager.HandleKey(h.t.Context(), key)
		h.advance(time.Second)
	}
}

// documents lists the finished PDFs, sorted by name.
func (h *harness) documents() []string {
	h.t.Helper()

	matches, err := filepath.Glob(filepath.Join(h.outDir, "*.pdf"))
	if err != nil {
		h.t.Fatal(err)
	}
	return matches
}

func (h *harness) readDocument(name string) string {
	h.t.Helper()

	content, err := os.ReadFile(filepath.Join(h.outDir, name))
	if err != nil {
		h.t.Fatalf("read document: %v", err)
	}
	return string(content)
}

func (h *harness) sessions() []store.Session {
	h.t.Helper()

	sessions, err := h.db.RecentSessions(100)
	if err != nil {
		h.t.Fatal(err)
	}
	return sessions
}

func (h *harness) actionKinds() []store.Kind {
	h.t.Helper()

	actions, err := h.db.RecentActions(100)
	if err != nil {
		h.t.Fatal(err)
	}

	// RecentActions is newest first; reading the log forwards is easier.
	kinds := make([]store.Kind, 0, len(actions))
	for i := len(actions) - 1; i >= 0; i-- {
		kinds = append(kinds, actions[i].Kind)
	}
	return kinds
}

func (h *harness) workDirs() []string {
	h.t.Helper()

	matches, err := filepath.Glob(filepath.Join(h.workDir, "session-*"))
	if err != nil {
		h.t.Fatal(err)
	}
	return matches
}

func TestScanAndFinishStoresOneDocument(t *testing.T) {
	h := newHarness(t, nil)

	h.press('a', 'c')

	docs := h.documents()
	if len(docs) != 1 {
		t.Fatalf("got %d documents, want 1", len(docs))
	}
	// The name is the timestamp of the moment the session started.
	if want := "20260908-114400.pdf"; filepath.Base(docs[0]) != want {
		t.Fatalf("document name = %s, want %s", filepath.Base(docs[0]), want)
	}
	if got := h.readDocument("20260908-114400.pdf"); got != "session 1 page 1\n" {
		t.Fatalf("document content = %q", got)
	}

	sessions := h.sessions()
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if sessions[0].Status != store.StatusSaved || sessions[0].Pages != 1 {
		t.Fatalf("session = %+v, want saved with 1 page", sessions[0])
	}
	if filepath.Base(sessions[0].OutputPath) != "20260908-114400.pdf" {
		t.Fatalf("recorded output path = %q", sessions[0].OutputPath)
	}

	// Nothing may be left in the scratch directory.
	if dirs := h.workDirs(); len(dirs) != 0 {
		t.Fatalf("work directories left behind: %v", dirs)
	}
}

func TestKeyBAddsPagesInOrder(t *testing.T) {
	h := newHarness(t, nil)

	h.press('a', 'b', 'b', 'c')

	if h.scanner.count() != 3 {
		t.Fatalf("scanned %d pages, want 3", h.scanner.count())
	}

	want := "session 1 page 1\nsession 1 page 2\nsession 1 page 3\n"
	if got := h.readDocument("20260908-114400.pdf"); got != want {
		t.Fatalf("document content = %q, want %q", got, want)
	}

	if len(h.merger.calls) != 1 || len(h.merger.calls[0]) != 3 {
		t.Fatalf("merger calls = %v, want one call with 3 pages", h.merger.calls)
	}
	for i, page := range h.merger.calls[0] {
		if want := fmt.Sprintf("page-%03d.pdf", i+1); filepath.Base(page) != want {
			t.Fatalf("page %d = %s, want %s", i, filepath.Base(page), want)
		}
	}
}

func TestKeyAFinishesTheOpenDocumentFirst(t *testing.T) {
	h := newHarness(t, nil)

	// Two pages into the first document, then "a" starts the next one.
	h.press('a', 'b', 'a')

	docs := h.documents()
	if len(docs) != 1 {
		t.Fatalf("got %d documents, want 1 stored by the second 'a'", len(docs))
	}
	want := "session 1 page 1\nsession 1 page 2\n"
	if got := h.readDocument(filepath.Base(docs[0])); got != want {
		t.Fatalf("document content = %q, want %q", got, want)
	}

	state := h.manager.State()
	if !state.SessionActive || state.SessionID != 2 || state.Pages != 1 {
		t.Fatalf("state = %+v, want a new session #2 with 1 page", state)
	}

	// And finishing it stores the second document under its own name.
	h.press('c')
	if docs := h.documents(); len(docs) != 2 {
		t.Fatalf("got %d documents, want 2", len(docs))
	}
}

func TestKeyAOnEmptySessionDoesNotStoreAnything(t *testing.T) {
	h := newHarness(t, nil)
	h.scanner.errs[1] = errors.New("lamp not ready")

	// The first scan fails, so the session holds no pages; "a" must not
	// store an empty document, it just tries again.
	h.press('a', 'a')

	if docs := h.documents(); len(docs) != 0 {
		t.Fatalf("documents = %v, want none", docs)
	}
	if sessions := h.sessions(); len(sessions) != 1 {
		t.Fatalf("got %d sessions, want the failed one to be reused", len(sessions))
	}

	state := h.manager.State()
	if !state.SessionActive || state.Pages != 1 {
		t.Fatalf("state = %+v, want the retry to have produced a page", state)
	}
}

func TestKeyBWithoutSessionStartsOne(t *testing.T) {
	h := newHarness(t, nil)

	h.press('b')

	state := h.manager.State()
	if !state.SessionActive || state.SessionID != 1 || state.Pages != 1 {
		t.Fatalf("state = %+v, want an open session with 1 page", state)
	}

	h.press('c')
	if docs := h.documents(); len(docs) != 1 {
		t.Fatalf("got %d documents, want 1", len(docs))
	}
}

func TestKeyCWithoutSessionDoesNothing(t *testing.T) {
	h := newHarness(t, nil)

	h.press('c')

	if docs := h.documents(); len(docs) != 0 {
		t.Fatalf("documents = %v, want none", docs)
	}
	if sessions := h.sessions(); len(sessions) != 0 {
		t.Fatalf("sessions = %v, want none", sessions)
	}
	if state := h.manager.State(); state.Status != session.StatusIdle {
		t.Fatalf("status = %q, want idle", state.Status)
	}
}

func TestEmptySessionIsDiscarded(t *testing.T) {
	h := newHarness(t, nil)
	h.scanner.errs[1] = errors.New("paper jam")

	h.press('b', 'c')

	if docs := h.documents(); len(docs) != 0 {
		t.Fatalf("documents = %v, want none", docs)
	}

	sessions := h.sessions()
	if len(sessions) != 1 || sessions[0].Status != store.StatusDiscarded {
		t.Fatalf("sessions = %+v, want one discarded session", sessions)
	}
	if dirs := h.workDirs(); len(dirs) != 0 {
		t.Fatalf("work directories left behind: %v", dirs)
	}

	want := []store.Kind{
		store.KindKeyPressed, store.KindSessionStarted, store.KindScanFailed,
		store.KindKeyPressed, store.KindSessionDiscarded,
	}
	if got := h.actionKinds(); !equalKinds(got, want) {
		t.Fatalf("action log = %v, want %v", got, want)
	}
}

// TestDiscardDropsASessionWithPages verifies that ActionDiscard, unlike
// finishing an empty session, throws away pages that were already scanned
// rather than storing them.
func TestDiscardDropsASessionWithPages(t *testing.T) {
	h := newHarness(t, nil)

	h.press('a', 'b')
	if h.scanner.count() != 2 {
		t.Fatalf("scanned %d pages, want 2", h.scanner.count())
	}

	if err := h.manager.Do(t.Context(), session.ActionDiscard); err != nil {
		t.Fatalf("Do(discard): %v", err)
	}

	if docs := h.documents(); len(docs) != 0 {
		t.Fatalf("documents = %v, want none", docs)
	}

	sessions := h.sessions()
	if len(sessions) != 1 || sessions[0].Status != store.StatusDiscarded || sessions[0].Pages != 2 {
		t.Fatalf("sessions = %+v, want one discarded session with 2 pages", sessions)
	}
	if dirs := h.workDirs(); len(dirs) != 0 {
		t.Fatalf("work directories left behind: %v", dirs)
	}

	state := h.manager.State()
	if state.SessionActive {
		t.Fatalf("state = %+v, want no active session after discard", state)
	}

	actions, err := h.db.RecentActions(1)
	if err != nil {
		t.Fatal(err)
	}
	if actions[0].Kind != store.KindSessionDiscarded || actions[0].Page != 2 {
		t.Fatalf("last action = %+v, want a discard with 2 pages", actions[0])
	}
}

// TestDiscardWithNoActiveSessionIsANoop verifies that discarding without an
// open session does nothing rather than erroring or logging a bogus entry.
func TestDiscardWithNoActiveSessionIsANoop(t *testing.T) {
	h := newHarness(t, nil)

	if err := h.manager.Do(t.Context(), session.ActionDiscard); err != nil {
		t.Fatalf("Do(discard): %v", err)
	}

	if actions := h.actionKinds(); len(actions) != 0 {
		t.Fatalf("action log = %v, want none", actions)
	}
}

func TestFailedScanKeepsTheSessionOpen(t *testing.T) {
	h := newHarness(t, nil)
	h.scanner.errs[2] = errors.New("feeder empty")

	// Page 1 works, page 2 fails, retrying page 2 works.
	h.press('a', 'b')

	state := h.manager.State()
	if state.Status != session.StatusError {
		t.Fatalf("status = %q, want error", state.Status)
	}
	if !strings.Contains(state.LastError, "feeder empty") {
		t.Fatalf("last error = %q", state.LastError)
	}
	if !state.SessionActive || state.Pages != 1 {
		t.Fatalf("state = %+v, want the session to stay open with its page", state)
	}

	h.press('b', 'c')

	want := "session 1 page 1\nsession 1 page 2\n"
	if got := h.readDocument("20260908-114400.pdf"); got != want {
		t.Fatalf("document content = %q, want %q", got, want)
	}
	if state := h.manager.State(); state.LastError != "" {
		t.Fatalf("last error = %q, want it cleared after a good scan", state.LastError)
	}
}

func TestFailedMergeKeepsScannedPages(t *testing.T) {
	h := newHarness(t, nil)
	h.merger.err = errors.New("out of disk space")

	h.press('a', 'b', 'c')

	if docs := h.documents(); len(docs) != 0 {
		t.Fatalf("documents = %v, want none", docs)
	}

	sessions := h.sessions()
	if len(sessions) != 1 || sessions[0].Status != store.StatusFailed {
		t.Fatalf("sessions = %+v, want one failed session", sessions)
	}
	if !strings.Contains(sessions[0].Error, "out of disk space") {
		t.Fatalf("recorded error = %q", sessions[0].Error)
	}

	// The pages must survive so they can be rescued by hand.
	dirs := h.workDirs()
	if len(dirs) != 1 {
		t.Fatalf("work directories = %v, want the pages to be kept", dirs)
	}
	pages, err := filepath.Glob(filepath.Join(dirs[0], "*.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 {
		t.Fatalf("kept %d pages, want 2", len(pages))
	}

	if state := h.manager.State(); state.SessionActive {
		t.Fatalf("state = %+v, want the failed session to be closed", state)
	}
}

func TestScanOverlapsWithProcessing(t *testing.T) {
	scanner := newFakePipelinedScanner()
	scanner.gate[1] = make(chan struct{})

	h := newHarness(t, func(o *session.Options) { o.Scanner = scanner })

	h.press('a')
	if n := scanner.acquiredCount(); n != 1 {
		t.Fatalf("acquired = %d, want 1", n)
	}

	// Page 1's processing is blocked on its gate; acquiring page 2 must not
	// wait for it.
	h.press('b')
	if n := scanner.acquiredCount(); n != 2 {
		t.Fatalf("acquired = %d, want 2 (page 2 acquired while page 1 was still processing)", n)
	}

	// Page 2 has no gate, so it finishes processing right away, proving the
	// two pages' processing steps run independently of each other.
	if req := <-scanner.done; req.Page != 2 {
		t.Fatalf("first page to finish processing = %d, want 2", req.Page)
	}
	if scanner.hasProcessed(1) {
		t.Fatal("page 1 was processed despite its gate still being closed")
	}

	close(scanner.gate[1])
	if req := <-scanner.done; req.Page != 1 {
		t.Fatalf("second page to finish processing = %d, want 1", req.Page)
	}

	h.press('c')

	// Pages are recorded in acquisition order, so the document comes out in
	// the right order regardless of which one finished processing first.
	want := "session 1 page 1\nsession 1 page 2\n"
	if got := h.readDocument("20260908-114400.pdf"); got != want {
		t.Fatalf("document content = %q, want %q", got, want)
	}
}

func TestBackgroundProcessingFailureAbortsSession(t *testing.T) {
	scanner := newFakePipelinedScanner()
	scanner.processErrs[1] = errors.New("disk full")

	h := newHarness(t, func(o *session.Options) { o.Scanner = scanner })

	h.press('a')
	<-scanner.done // wait for page 1's background processing to finish (and fail)

	state := h.manager.State()
	if state.Status != session.StatusError {
		t.Fatalf("status = %q, want error", state.Status)
	}
	if !strings.Contains(state.LastError, "disk full") {
		t.Fatalf("last error = %q", state.LastError)
	}

	// The session cannot produce a valid document anymore: further scans
	// into it are refused,
	if err := h.manager.Do(t.Context(), session.ActionScanPage); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("scan after abort: err = %v, want the abort error", err)
	}
	// and "c" fails it instead of merging a document silently missing its
	// page.
	if err := h.manager.Do(t.Context(), session.ActionFinish); err == nil {
		t.Fatal("finish after abort: want an error")
	}

	if docs := h.documents(); len(docs) != 0 {
		t.Fatalf("documents = %v, want none", docs)
	}

	sessions := h.sessions()
	if len(sessions) != 1 || sessions[0].Status != store.StatusFailed {
		t.Fatalf("sessions = %+v, want one failed session", sessions)
	}

	foundProcessFailed := false
	for _, k := range h.actionKinds() {
		if k == store.KindProcessFailed {
			foundProcessFailed = true
		}
	}
	if !foundProcessFailed {
		t.Fatal("action log missing a process-failed entry")
	}

	if state := h.manager.State(); state.SessionActive {
		t.Fatalf("state = %+v, want the aborted session to be closed", state)
	}
}

func TestOutputNamesDoNotCollide(t *testing.T) {
	// A clock that never moves forces two documents onto the same name.
	h := newHarness(t, nil)

	h.manager.HandleKey(t.Context(), 'a')
	h.manager.HandleKey(t.Context(), 'c')
	h.manager.HandleKey(t.Context(), 'a')
	h.manager.HandleKey(t.Context(), 'c')

	docs := h.documents()
	if len(docs) != 2 {
		t.Fatalf("got %d documents, want 2", len(docs))
	}

	names := []string{filepath.Base(docs[0]), filepath.Base(docs[1])}
	want := []string{"20260908-114400-2.pdf", "20260908-114400.pdf"}
	if names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("document names = %v, want %v", names, want)
	}
}

func TestCloseStoresTheOpenDocument(t *testing.T) {
	h := newHarness(t, nil)

	h.press('a', 'b')

	if err := h.manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if docs := h.documents(); len(docs) != 1 {
		t.Fatalf("got %d documents, want the open one to be stored on shutdown", len(docs))
	}
	if sessions := h.sessions(); sessions[0].Status != store.StatusSaved {
		t.Fatalf("session status = %q, want saved", sessions[0].Status)
	}
}

func TestUnknownKeysAreIgnored(t *testing.T) {
	h := newHarness(t, nil)

	h.press('x', '\n', 'q')

	if h.scanner.count() != 0 {
		t.Fatalf("scanned %d pages, want 0", h.scanner.count())
	}
	if sessions := h.sessions(); len(sessions) != 0 {
		t.Fatalf("sessions = %v, want none", sessions)
	}

	want := []store.Kind{store.KindKeyIgnored, store.KindKeyIgnored, store.KindKeyIgnored}
	if got := h.actionKinds(); !equalKinds(got, want) {
		t.Fatalf("action log = %v, want %v", got, want)
	}
}

func TestUppercaseKeysWork(t *testing.T) {
	h := newHarness(t, nil)

	h.press('A', 'B', 'C')

	if docs := h.documents(); len(docs) != 1 {
		t.Fatalf("got %d documents, want 1", len(docs))
	}
	if h.scanner.count() != 2 {
		t.Fatalf("scanned %d pages, want 2", h.scanner.count())
	}
}

func TestActionForKey(t *testing.T) {
	for key, want := range map[rune]session.Action{
		'a': session.ActionScanNew,
		'A': session.ActionScanNew,
		'b': session.ActionScanPage,
		'B': session.ActionScanPage,
		'c': session.ActionFinish,
		'C': session.ActionFinish,
	} {
		got, ok := session.ActionForKey(key)
		if !ok || got != want {
			t.Errorf("ActionForKey(%q) = %q, %v, want %q, true", key, got, ok, want)
		}
	}

	for _, key := range []rune{'d', 'x', '1', ' ', '\n'} {
		if _, ok := session.ActionForKey(key); ok {
			t.Errorf("ActionForKey(%q) = bound, want unbound", key)
		}
	}
}

func TestStatusDuringScan(t *testing.T) {
	h := newHarness(t, nil)

	var seen session.Status
	h.scanner.hook = func(scan.Request) {
		seen = h.manager.State().Status
	}

	h.press('a')

	if seen != session.StatusScanning {
		t.Fatalf("status during scan = %q, want scanning", seen)
	}
	if got := h.manager.State().Status; got != session.StatusIdle {
		t.Fatalf("status after scan = %q, want idle", got)
	}
}

func TestActionsAreSerialised(t *testing.T) {
	h := newHarness(t, nil)

	var (
		inFlight atomic.Int32
		overlaps atomic.Int32
	)
	h.scanner.hook = func(scan.Request) {
		if inFlight.Add(1) > 1 {
			overlaps.Add(1)
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
	}

	const presses = 8
	var wg sync.WaitGroup
	for range presses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.manager.Do(t.Context(), session.ActionScanPage)
		}()
	}
	wg.Wait()

	if overlaps.Load() != 0 {
		t.Fatalf("%d overlapping scans, want the action loop to serialise them", overlaps.Load())
	}
	if got := h.manager.State().Pages; got != presses {
		t.Fatalf("pages = %d, want %d", got, presses)
	}
}

func TestEndToEndProducesARealPDF(t *testing.T) {
	// Same flow as above, but with real PDF pages and the real merger, so
	// the output is a document a PDF reader would accept.
	root := t.TempDir()
	outDir := filepath.Join(root, "scans")

	db, err := store.Open(filepath.Join(root, "scantool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	shade := uint8(0)
	scanner := scan.Func(func(_ context.Context, req scan.Request) error {
		shade += 80
		testpdf.Write(t, req.Dest, shade)
		return nil
	})

	manager, err := session.New(session.Options{
		OutDir:   outDir,
		Scanner:  scanner,
		Merger:   &pdfmerge.PDFCPU{},
		Recorder: db,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []rune{'a', 'b', 'b', 'c'} {
		if err := manager.HandleKey(t.Context(), key); err != nil {
			t.Fatalf("key %q: %v", key, err)
		}
	}

	docs, err := filepath.Glob(filepath.Join(outDir, "*.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d documents, want 1", len(docs))
	}

	testpdf.Validate(t, docs[0])
	if pages := testpdf.PageCount(t, docs[0]); pages != 3 {
		t.Fatalf("document has %d pages, want 3", pages)
	}
}

// brokenRecorder fails on everything except creating a session.
type brokenRecorder struct{ session.Recorder }

func (brokenRecorder) CreateSessionWithAction(time.Time) (int64, error) { return 99, nil }
func (brokenRecorder) AddPage(int64, int, string, store.Action) error   { return errors.New("db gone") }
func (brokenRecorder) AppendAction(store.Action) (int64, error)         { return 0, errors.New("db gone") }
func (brokenRecorder) SetActionSessionID(int64, int64) error            { return errors.New("db gone") }
func (brokenRecorder) ActiveSession() (store.Session, error)            { return store.Session{}, sql.ErrNoRows }
func (brokenRecorder) SessionPages(int64) ([]string, error)             { return nil, nil }

func (brokenRecorder) FinishSessionWithAction(int64, time.Time, string, string, string, store.Action) error {
	return errors.New("db gone")
}

func TestScanningSurvivesABrokenDatabase(t *testing.T) {
	h := newHarness(t, func(o *session.Options) { o.Recorder = brokenRecorder{} })

	h.press('a', 'b', 'c')

	// Losing the log must never lose the document.
	if docs := h.documents(); len(docs) != 1 {
		t.Fatalf("got %d documents, want 1 despite the database errors", len(docs))
	}
	if got := h.readDocument("20260908-114400.pdf"); got != "session 99 page 1\nsession 99 page 2\n" {
		t.Fatalf("document content = %q", got)
	}
}

type failingRecorder struct{ session.Recorder }

func (failingRecorder) CreateSessionWithAction(time.Time) (int64, error) {
	return 0, errors.New("disk is read only")
}

func (failingRecorder) ActiveSession() (store.Session, error) { return store.Session{}, sql.ErrNoRows }

func TestSessionCannotStartWithoutRecorder(t *testing.T) {
	h := newHarness(t, func(o *session.Options) { o.Recorder = failingRecorder{} })

	err := h.manager.Do(t.Context(), session.ActionScanPage)
	if err == nil {
		t.Fatal("want error when the session cannot be recorded, got nil")
	}
	if h.scanner.count() != 0 {
		t.Fatal("nothing should have been scanned")
	}
	if state := h.manager.State(); state.Status != session.StatusError {
		t.Fatalf("status = %q, want error", state.Status)
	}
}

// TestRecoverActiveSessionAfterRestart verifies that a new Manager, backed
// by the same database and work directory as a crashed one, picks up an
// active session from the database rather than starting fresh: the database
// is the source of truth, not the process.
func TestRecoverActiveSessionAfterRestart(t *testing.T) {
	h := newHarness(t, nil)
	h.press('a', 'b')

	state := h.manager.State()
	if !state.SessionActive || state.Pages != 2 {
		t.Fatalf("state = %+v, want an active session with 2 pages", state)
	}

	// Simulate a crash: build a new Manager against the same store and work
	// directory without ever calling Close on the old one.
	manager2, err := session.New(session.Options{
		OutDir:   h.outDir,
		WorkDir:  h.workDir,
		Scanner:  h.scanner,
		Merger:   h.merger,
		Recorder: h.db,
		Now:      h.clock,
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}

	recovered := manager2.State()
	if !recovered.SessionActive {
		t.Fatal("recovered manager has no active session")
	}
	if recovered.SessionID != state.SessionID || recovered.Pages != 2 {
		t.Fatalf("recovered state = %+v, want session %d with 2 pages", recovered, state.SessionID)
	}

	// The recovered session must behave like the original: finishing it
	// stores exactly the pages that were scanned before the "crash".
	if err := manager2.Do(t.Context(), session.ActionFinish); err != nil {
		t.Fatalf("finish recovered session: %v", err)
	}
	if docs := h.documents(); len(docs) != 1 {
		t.Fatalf("got %d documents, want 1", len(docs))
	}
}

// TestRecoverActiveSessionWithoutWorkDirFailsIt verifies that a session
// whose scratch directory is gone (e.g. the disk holding it was wiped) gets
// recorded as failed instead of staying active in the database forever.
func TestRecoverActiveSessionWithoutWorkDirFailsIt(t *testing.T) {
	h := newHarness(t, nil)
	h.press('a', 'b')

	for _, dir := range h.workDirs() {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}

	manager2, err := session.New(session.Options{
		OutDir:   h.outDir,
		WorkDir:  h.workDir,
		Scanner:  h.scanner,
		Merger:   h.merger,
		Recorder: h.db,
		Now:      h.clock,
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}

	if state := manager2.State(); state.SessionActive {
		t.Fatalf("state = %+v, want no active session", state)
	}

	sessions := h.sessions()
	if len(sessions) != 1 || sessions[0].Status != store.StatusFailed {
		t.Fatalf("sessions = %+v, want a single failed session", sessions)
	}
}

// TestFinishUploadsTheDocumentAndRecordsSuccess verifies that a configured
// Uploader receives the finished document and that the outcome is recorded
// on the session, in the action log, and that the document is kept on disk
// so a failed upload could be retried later.
func TestFinishUploadsTheDocumentAndRecordsSuccess(t *testing.T) {
	uploader := &fakeUploader{id: "record-123"}
	h := newHarness(t, func(o *session.Options) { o.Uploader = uploader })

	h.press('a', 'b', 'c')

	sessions := h.sessions()
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	if sessions[0].UploadStatus != store.UploadUploaded {
		t.Fatalf("upload_status = %q, want %q", sessions[0].UploadStatus, store.UploadUploaded)
	}
	if sessions[0].LemmaryID != "record-123" {
		t.Fatalf("lemmary_id = %q, want record-123", sessions[0].LemmaryID)
	}

	docs := h.documents()
	if len(docs) != 1 {
		t.Fatalf("got %d documents, want 1", len(docs))
	}
	if got := uploader.uploaded(); len(got) != 1 || got[0] != docs[0] {
		t.Fatalf("uploaded = %v, want [%s]", got, docs[0])
	}

	kinds := h.actionKinds()
	if kinds[len(kinds)-1] != store.KindUploadSucceeded {
		t.Fatalf("last action = %q, want %q", kinds[len(kinds)-1], store.KindUploadSucceeded)
	}

	// The document must still be on disk: a failed upload must be
	// retryable, and this one did not even fail.
	if _, err := os.Stat(docs[0]); err != nil {
		t.Fatalf("document missing after upload: %v", err)
	}
}

// TestFinishKeepsDocumentWhenUploadFails verifies that a failed upload does
// not remove the document and is recorded as failed rather than silently
// dropped.
func TestFinishKeepsDocumentWhenUploadFails(t *testing.T) {
	uploader := &fakeUploader{err: errors.New("server unreachable")}
	h := newHarness(t, func(o *session.Options) { o.Uploader = uploader })

	h.press('a', 'b', 'c')

	docs := h.documents()
	if len(docs) != 1 {
		t.Fatalf("got %d documents, want 1", len(docs))
	}
	if _, err := os.Stat(docs[0]); err != nil {
		t.Fatalf("document missing after failed upload: %v", err)
	}

	sessions := h.sessions()
	if len(sessions) != 1 || sessions[0].UploadStatus != store.UploadFailed {
		t.Fatalf("sessions = %+v, want a single session with upload_status %q", sessions, store.UploadFailed)
	}
	if sessions[0].UploadError == "" {
		t.Fatal("upload_error was not recorded")
	}

	kinds := h.actionKinds()
	if kinds[len(kinds)-1] != store.KindUploadFailed {
		t.Fatalf("last action = %q, want %q", kinds[len(kinds)-1], store.KindUploadFailed)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	base := func() session.Options {
		return session.Options{
			OutDir:  t.TempDir(),
			Scanner: &fakeScanner{},
			Merger:  &fakeMerger{},
		}
	}

	tests := map[string]func(*session.Options){
		"missing out dir": func(o *session.Options) { o.OutDir = "" },
		"missing scanner": func(o *session.Options) { o.Scanner = nil },
		"missing merger":  func(o *session.Options) { o.Merger = nil },
	}

	for name, break_ := range tests {
		opts := base()
		break_(&opts)
		if _, err := session.New(opts); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}

	if _, err := session.New(base()); err != nil {
		t.Errorf("valid options: %v", err)
	}
}

func TestUnknownActionIsRejected(t *testing.T) {
	h := newHarness(t, nil)

	if err := h.manager.Do(t.Context(), session.Action("dance")); err == nil {
		t.Fatal("want error for an unknown action, got nil")
	}
}

func TestActionLogOfAFullDocument(t *testing.T) {
	h := newHarness(t, nil)

	h.press('a', 'b', 'c')

	want := []store.Kind{
		store.KindKeyPressed, store.KindSessionStarted, store.KindPageScanned,
		store.KindKeyPressed, store.KindPageScanned,
		store.KindKeyPressed, store.KindSessionSaved,
	}
	if got := h.actionKinds(); !equalKinds(got, want) {
		t.Fatalf("action log = %v, want %v", got, want)
	}

	actions, err := h.db.RecentActions(1)
	if err != nil {
		t.Fatal(err)
	}
	saved := actions[0]
	if saved.SessionID != 1 || saved.Page != 2 {
		t.Fatalf("stored action = %+v, want session 1 with 2 pages", saved)
	}
	if filepath.Base(saved.Detail) != "20260908-114400.pdf" {
		t.Fatalf("stored action detail = %q, want the document path", saved.Detail)
	}
}

// TestKeyPressStartingASessionIsAttachedToIt verifies that the key press
// which starts a session from idle (the common case: pressing "b" to scan
// the first page) is recorded against the session it created, rather than
// being logged as a session-less entry, since it happened before the
// session existed.
func TestKeyPressStartingASessionIsAttachedToIt(t *testing.T) {
	h := newHarness(t, nil)

	h.press('b')

	actions, err := h.db.RecentActions(100)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if a.Kind == store.KindKeyPressed && a.SessionID == 0 {
			t.Fatalf("key press %+v has no session, want it attached to session 1", a)
		}
	}
}

func equalKinds(got, want []store.Kind) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
