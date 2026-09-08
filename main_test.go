package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oliverbestmann/scantool/internal/keys"
	"github.com/oliverbestmann/scantool/internal/pdfmerge"
	"github.com/oliverbestmann/scantool/internal/scan"
	"github.com/oliverbestmann/scantool/internal/session"
	"github.com/oliverbestmann/scantool/internal/store"
	"github.com/oliverbestmann/scantool/internal/testpdf"
	"github.com/oliverbestmann/scantool/internal/web"
)

// waitFor polls until cond holds, which keeps the tests free of fixed sleeps.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// fakeScanScript stands in for scan-page.sh: there is no SANE here, so it
// just copies a prepared single page PDF to the requested destination.
func fakeScanScript(t *testing.T, dir string) string {
	t.Helper()

	fixture := filepath.Join(dir, "fixture.pdf")
	testpdf.Write(t, fixture, 128)

	script := filepath.Join(dir, "scan-page.sh")
	body := "#!/bin/sh\nsleep 0.01\ncp " + fixture + " \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// TestDaemonEndToEnd wires up the same pieces as main and drives the daemon
// through a full document: key presses in, a real multi page PDF out, with
// the web UI reporting what happened.
func TestDaemonEndToEnd(t *testing.T) {
	root := t.TempDir()
	outDir := filepath.Join(root, "scans")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	db, err := store.Open(filepath.Join(root, "scantool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	manager, err := session.New(session.Options{
		OutDir:   outDir,
		Scanner:  &scan.ShellScanner{Command: fakeScanScript(t, root), Timeout: 30 * time.Second},
		Merger:   &pdfmerge.PDFCPU{},
		Recorder: db,
		Logger:   logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	keyCh := make(chan rune, 32)
	actionCh := make(chan session.Action, 32)
	sourceDone := make(chan error, 1)

	loopDone := make(chan error, 1)
	go func() { loopDone <- loop(ctx, manager, keyCh, actionCh, sourceDone, logger) }()

	// The keyboard is a pipe here, everything behind it is the real thing.
	keyboard, typist, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer typist.Close()
	go func() { sourceDone <- (&keys.Stdin{In: keyboard, Raw: false}).Run(ctx, keyCh) }()

	server, err := web.New(web.Options{
		State:  manager.State,
		Reader: db,
		Logger: logger,
		Submit: func(a session.Action) error {
			actionCh <- a
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ui := httptest.NewServer(server.Handler())
	defer ui.Close()

	// "a" opens a document, "b" adds two more pages.
	if _, err := typist.WriteString("abb"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "three scanned pages", func() bool { return manager.State().Pages == 3 })

	// The web UI sees the open session.
	state := fetchState(t, ui.URL)
	if !state.State.SessionActive || state.State.Pages != 3 {
		t.Fatalf("web state = %+v, want an open session with 3 pages", state.State)
	}

	// A fourth page, this time triggered from the browser.
	res, err := http.Post(ui.URL+"/api/action?key=b", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/action: status = %d, want 202", res.StatusCode)
	}
	waitFor(t, "the page requested by the web ui", func() bool { return manager.State().Pages == 4 })

	// "c" stores the document.
	if _, err := typist.WriteString("c"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the stored document", func() bool { return manager.State().LastSavedPath != "" })

	doc := manager.State().LastSavedPath
	testpdf.Validate(t, doc)
	if pages := testpdf.PageCount(t, doc); pages != 4 {
		t.Fatalf("document has %d pages, want 4", pages)
	}
	if filepath.Dir(doc) != outDir {
		t.Fatalf("document stored in %s, want %s", filepath.Dir(doc), outDir)
	}

	// And the web UI reports it as a finished document.
	state = fetchState(t, ui.URL)
	if len(state.Sessions) != 1 {
		t.Fatalf("web sessions = %+v, want 1", state.Sessions)
	}
	if state.Sessions[0].Status != store.StatusSaved || state.Sessions[0].Pages != 4 {
		t.Fatalf("web session = %+v, want saved with 4 pages", state.Sessions[0])
	}
	if len(state.Actions) == 0 || state.Actions[0].Kind != store.KindSessionSaved {
		t.Fatalf("newest action = %+v, want the stored document", state.Actions)
	}

	cancel()
	select {
	case err := <-loopDone:
		if err != nil {
			t.Fatalf("loop: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the action loop did not stop")
	}
}

// TestDaemonStoresAnOpenDocumentOnShutdown covers the shutdown path: pages
// that were already scanned must not be lost when the daemon stops.
func TestDaemonStoresAnOpenDocumentOnShutdown(t *testing.T) {
	root := t.TempDir()
	outDir := filepath.Join(root, "scans")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	db, err := store.Open(filepath.Join(root, "scantool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	manager, err := session.New(session.Options{
		OutDir:   outDir,
		Scanner:  &scan.ShellScanner{Command: fakeScanScript(t, root)},
		Merger:   &pdfmerge.PDFCPU{},
		Recorder: db,
		Logger:   logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	keyCh := make(chan rune, 8)
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- loop(ctx, manager, keyCh, make(chan session.Action), make(chan error, 1), logger)
	}()

	keyCh <- 'a'
	keyCh <- 'b'
	waitFor(t, "two scanned pages", func() bool { return manager.State().Pages == 2 })

	cancel()
	if err := <-loopDone; err != nil {
		t.Fatalf("loop: %v", err)
	}

	// This is what run() does after the loop returns.
	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	docs, err := filepath.Glob(filepath.Join(outDir, "*.pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d documents, want the open one to be stored", len(docs))
	}
	if pages := testpdf.PageCount(t, docs[0]); pages != 2 {
		t.Fatalf("document has %d pages, want 2", pages)
	}
}

func TestLoopStopsWhenTheKeySourceQuits(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	manager, err := session.New(session.Options{
		OutDir:  filepath.Join(t.TempDir(), "scans"),
		Scanner: scan.Func(func(context.Context, scan.Request) error { return nil }),
		Merger:  pdfmerge.Func(func([]string, string) error { return nil }),
		Logger:  logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	sourceDone := make(chan error, 1)
	sourceDone <- keys.ErrQuit

	done := make(chan error, 1)
	go func() {
		done <- loop(t.Context(), manager, make(chan rune), make(chan session.Action), sourceDone, logger)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("loop: %v, want nil after a quit request", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not stop")
	}
}

func TestLoopReportsAFailingKeySource(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	manager, err := session.New(session.Options{
		OutDir:  filepath.Join(t.TempDir(), "scans"),
		Scanner: scan.Func(func(context.Context, scan.Request) error { return nil }),
		Merger:  pdfmerge.Func(func([]string, string) error { return nil }),
		Logger:  logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	sourceDone := make(chan error, 1)
	sourceDone <- os.ErrPermission

	err = loop(t.Context(), manager, make(chan rune), make(chan session.Action), sourceDone, logger)
	if err == nil {
		t.Fatal("want the loop to report a broken key source, got nil")
	}
}

func TestNewMerger(t *testing.T) {
	if _, ok := newMerger("").(*pdfmerge.PDFCPU); !ok {
		t.Error("empty command should use the built-in merger")
	}

	merger, ok := newMerger("pdfunite {{in}} {{out}}").(*pdfmerge.Command)
	if !ok {
		t.Fatal("a command should use the external merger")
	}
	if merger.Name != "pdfunite" || len(merger.Args) != 2 {
		t.Fatalf("merger = %+v", merger)
	}
}

func TestNewKeySource(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	evdev, err := newKeySource(config{input: "evdev", devices: "/dev/input/event1, /dev/input/event2"}, logger)
	if err != nil {
		t.Fatal(err)
	}
	if got := evdev.Name(); got != "evdev /dev/input/event1,/dev/input/event2" {
		t.Errorf("Name() = %q", got)
	}

	if _, err := newKeySource(config{input: "libinput"}, logger); err != nil {
		t.Errorf("libinput: %v", err)
	}
	if _, err := newKeySource(config{input: "stdin"}, logger); err != nil {
		t.Errorf("stdin: %v", err)
	}
	if _, err := newKeySource(config{input: "telepathy"}, logger); err == nil {
		t.Error("unknown input source: want error, got nil")
	}
}

func TestResolveGrab(t *testing.T) {
	tests := []struct {
		mode     string
		explicit bool
		want     bool
	}{
		// "auto" never takes over a keyboard the user did not name.
		{mode: "auto", explicit: false, want: false},
		{mode: "auto", explicit: true, want: true},
		{mode: "", explicit: true, want: true},
		{mode: "yes", explicit: false, want: true},
		{mode: "no", explicit: true, want: false},
	}

	for _, tc := range tests {
		got, err := resolveGrab(tc.mode, tc.explicit)
		if err != nil {
			t.Errorf("resolveGrab(%q, %v): %v", tc.mode, tc.explicit, err)
			continue
		}
		if got != tc.want {
			t.Errorf("resolveGrab(%q, %v) = %v, want %v", tc.mode, tc.explicit, got, tc.want)
		}
	}

	if _, err := resolveGrab("maybe", false); err == nil {
		t.Error("invalid mode: want error, got nil")
	}
}

func TestNewKeySourceDoesNotGrabAutodetectedDevices(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	source, err := newKeySource(config{input: "evdev", grab: "auto"}, logger)
	if err != nil {
		t.Fatal(err)
	}
	evdev, ok := source.(*keys.Evdev)
	if !ok {
		t.Fatalf("source = %T, want *keys.Evdev", source)
	}
	if evdev.Grab {
		t.Error("auto-detected devices must not be grabbed by default")
	}
	if len(evdev.RequireKeys) == 0 {
		t.Error("auto-detected devices should be filtered by the keys we act on")
	}

	source, err = newKeySource(config{input: "evdev", grab: "auto", devices: "/dev/input/event1"}, logger)
	if err != nil {
		t.Fatal(err)
	}
	if !source.(*keys.Evdev).Grab {
		t.Error("a device named explicitly should be grabbed")
	}
}

func TestNewLogger(t *testing.T) {
	if _, err := newLogger("debug"); err != nil {
		t.Errorf("debug: %v", err)
	}
	if _, err := newLogger("nonsense"); err == nil {
		t.Error("invalid level: want error, got nil")
	}
}

type uiState struct {
	State    session.State   `json:"state"`
	Sessions []store.Session `json:"sessions"`
	Actions  []store.Action  `json:"actions"`
}

func fetchState(t *testing.T, base string) uiState {
	t.Helper()

	res, err := http.Get(base + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/state: status = %d", res.StatusCode)
	}

	var state uiState
	if err := json.NewDecoder(res.Body).Decode(&state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	return state
}
