// Command scantool is a small daemon for a headless Raspberry Pi: it listens
// for key presses on a USB keypad and turns them into scanned PDF documents.
//
//	a  start a new document and scan its first page
//	b  scan another page into the current document
//	c  finish the document and store it as <timestamp>.pdf
//
// Pressing "a" while a document already holds pages stores that document
// first. Pressing "b" without an open document starts one. An open document
// left untouched for --idle-timeout is finished automatically, as if "c"
// had been pressed. Stopping and restarting the daemon does not finish an
// open document either; it resumes where it left off.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/oliverbestmann/scantool/internal/keys"
	"github.com/oliverbestmann/scantool/internal/lemmary"
	"github.com/oliverbestmann/scantool/internal/pdfmerge"
	"github.com/oliverbestmann/scantool/internal/scan"
	"github.com/oliverbestmann/scantool/internal/session"
	"github.com/oliverbestmann/scantool/internal/store"
	"github.com/oliverbestmann/scantool/internal/web"
)

type config struct {
	outDir      string
	workDir     string
	dbPath      string
	scanCmd     string
	scanTimeout time.Duration
	devScanner  bool
	mergeCmd    string
	fileLayout  string
	idleTimeout time.Duration

	lemmaryURL        string
	lemmaryAPIKey     string
	lemmaryAPIKeyFile string
	devLemmary        bool

	input   string
	devices string
	grab    string

	httpAddr   string
	webControl bool

	logLevel    string
	keepActions int
	listDevices bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "scantool:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := parseFlags()

	logger, err := newLogger(cfg.logLevel)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	if cfg.listDevices {
		return listDevices()
	}

	if cfg.workDir == "" {
		cfg.workDir = filepath.Join(cfg.outDir, ".scantool-work")
	}
	if cfg.dbPath == "" {
		cfg.dbPath = filepath.Join(cfg.outDir, "scantool.db")
	}
	if cfg.devLemmary && cfg.lemmaryURL == "" {
		// There's no real server to link to; without this, the web UI could
		// never show what a finished document's lemmary link looks like
		// while testing --dev-lemmary.
		cfg.lemmaryURL = "https://lemmary.invalid"
	}
	if err := os.MkdirAll(cfg.outDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}

	db, err := store.Open(cfg.dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if cfg.keepActions > 0 {
		if err := db.TrimActions(cfg.keepActions); err != nil {
			logger.Warn("could not trim action log", "error", err)
		}
	}

	uploader, err := newUploader(cfg)
	if err != nil {
		return err
	}

	manager, err := session.New(session.Options{
		OutDir:     cfg.outDir,
		WorkDir:    cfg.workDir,
		FileLayout: cfg.fileLayout,
		Scanner:    newScanner(cfg),
		Merger:     newMerger(cfg.mergeCmd),
		Recorder:   db,
		Uploader:   uploader,
		Logger:     logger,
	})
	if err != nil {
		return err
	}

	source, err := newKeySource(cfg, logger)
	if err != nil {
		return err
	}

	// Signals stop the daemon. An open document is left as is: its pages are
	// already durably saved, so nothing is lost, and the next start resumes
	// it via session.New's recovery instead of finishing it prematurely.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Key presses and web requests both feed the single action loop, so only
	// one scan runs at a time. Both channels are unbuffered: presses and web
	// clicks that arrive while the loop is busy handling a previous one are
	// discarded (dropped for keys, rejected with an error for the web), not
	// queued.
	keyCh := make(chan rune)
	actionCh := make(chan session.Action)

	var submit func(session.Action) error
	if cfg.webControl {
		submit = func(a session.Action) error {
			select {
			case actionCh <- a:
				return nil
			default:
				return errors.New("scantool is busy, try again in a moment")
			}
		}
	}

	if cfg.httpAddr != "" {
		server, err := web.New(web.Options{
			Addr:       cfg.httpAddr,
			State:      manager.State,
			Reader:     db,
			Submit:     submit,
			LemmaryURL: cfg.lemmaryURL,
			Logger:     logger,
		})
		if err != nil {
			return err
		}
		go func() {
			if err := server.ListenAndServe(ctx); err != nil {
				logger.Error("web ui stopped", "error", err)
			}
		}()
	}

	// The key source runs on its own goroutine; a fatal error there stops
	// the daemon.
	sourceDone := make(chan error, 1)
	go func() { sourceDone <- source.Run(ctx, keyCh) }()

	scanCommand := cfg.scanCmd
	switch {
	case cfg.devScanner:
		scanCommand = "dev (fake, no hardware)"
	case scanCommand == "":
		scanCommand = "built-in (scanimage/imagemagick/img2pdf)"
	}
	logger.Info("scantool started",
		"input", source.Name(),
		"out", cfg.outDir,
		"scan_command", scanCommand,
		"http", cfg.httpAddr,
		"idle_timeout", cfg.idleTimeout)
	logger.Info("key bindings: a = new document, b = add page, c = finish document")

	manager.Record(store.Action{Kind: store.KindDaemonStarted, Detail: source.Name()})

	err = loop(ctx, manager, keyCh, actionCh, sourceDone, logger, cfg.idleTimeout)

	manager.Record(store.Action{Kind: store.KindDaemonStopped})
	logger.Info("scantool stopped")

	return err
}

// loop runs actions one at a time until the context is cancelled or the key
// source gives up. When idleTimeout is positive, a document that is still
// open after that much inactivity is finished automatically, just like
// pressing "c"; idleTimeout <= 0 disables this.
func loop(ctx context.Context, manager *session.Manager, keyCh <-chan rune, actionCh <-chan session.Action, sourceDone <-chan error, logger *slog.Logger, idleTimeout time.Duration) error {
	var idleTimer *time.Timer
	var idleC <-chan time.Time
	if idleTimeout > 0 {
		idleTimer = time.NewTimer(idleTimeout)
		defer idleTimer.Stop()
		idleC = idleTimer.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil

		case err := <-sourceDone:
			switch {
			case errors.Is(err, keys.ErrQuit):
				logger.Info("quit requested")
				return nil
			case err != nil:
				return fmt.Errorf("keyboard input: %w", err)
			default:
				logger.Info("keyboard input ended")
				return nil
			}

		case key := <-keyCh:
			if idleTimer != nil {
				idleTimer.Reset(idleTimeout)
			}
			// Errors are already logged and recorded by the manager; the
			// daemon keeps running so the next key press still works.
			if err := manager.HandleKey(ctx, key); err != nil {
				logger.Debug("action failed", "error", err)
			}

		case action := <-actionCh:
			if idleTimer != nil {
				idleTimer.Reset(idleTimeout)
			}
			if err := manager.DoLogged(ctx, action, store.KindWebAction, string(action)); err != nil {
				logger.Debug("action failed", "error", err)
			}

		case <-idleC:
			idleTimer.Reset(idleTimeout)
			if manager.State().SessionActive {
				logger.Info("idle timeout, finishing document", "timeout", idleTimeout)
				manager.Record(store.Action{Kind: store.KindIdleTimeout, SessionID: manager.State().SessionID})
				if err := manager.Do(ctx, session.ActionFinish); err != nil {
					logger.Debug("action failed", "error", err)
				}
			}
		}
	}
}

func parseFlags() config {
	var cfg config

	flag.StringVar(&cfg.outDir, "out", "scans", "directory for the finished PDF documents")
	flag.StringVar(&cfg.workDir, "work", "", "directory for pages of open documents (default <out>/.scantool-work)")
	flag.StringVar(&cfg.dbPath, "db", "", "SQLite database for sessions and the action log (default <out>/scantool.db)")
	flag.StringVar(&cfg.scanCmd, "scan-command", "", "external command scanning one page, called as <command> <output.pdf>; "+
		"when unset, scantool scans pages itself via scanimage, imagemagick and img2pdf")
	flag.DurationVar(&cfg.scanTimeout, "scan-timeout", 3*time.Minute, "abort a scan that takes longer than this")
	flag.BoolVar(&cfg.devScanner, "dev-scanner", false, "fake the scanner: wait 3s and produce a blank page instead of scanning, for development without hardware")
	flag.StringVar(&cfg.mergeCmd, "merge-command", "", "external merge command, e.g. \"pdfunite {{in}} {{out}}\" (default: built-in pdfcpu)")
	flag.StringVar(&cfg.fileLayout, "name-layout", "20060102-150405", "Go time layout for the output file name")
	flag.DurationVar(&cfg.idleTimeout, "idle-timeout", 5*time.Minute, "automatically finish the current document after this much inactivity, 0 to disable")

	flag.StringVar(&cfg.lemmaryURL, "lemmary-url", "", "lemmary server to upload finished documents to, e.g. https://lemmary.example.com")
	flag.StringVar(&cfg.lemmaryAPIKey, "lemmary-api-key", "", "API key for the lemmary server (required together with --lemmary-url)")
	flag.StringVar(&cfg.lemmaryAPIKeyFile, "lemmary-api-key-file", "", "path to a file containing the lemmary API key, instead of --lemmary-api-key")
	flag.BoolVar(&cfg.devLemmary, "dev-lemmary", false, "fake the lemmary upload instead of using --lemmary-url: no server needed, fails 20% of requests to test retries; "+
		"the web UI links to a fake https://lemmary.invalid unless --lemmary-url overrides it")

	flag.StringVar(&cfg.input, "input", "evdev", "key source: evdev, libinput or stdin")
	flag.StringVar(&cfg.devices, "device", "", "comma separated input devices (default: all keyboards)")
	flag.StringVar(&cfg.grab, "grab", "auto", "take exclusive control of the input devices (evdev only): auto, yes or no")

	flag.StringVar(&cfg.httpAddr, "http", ":8080", "listen address of the status page, empty to disable")
	flag.BoolVar(&cfg.webControl, "web-control", true, "allow triggering a, b and c from the status page")

	flag.StringVar(&cfg.logLevel, "log-level", "info", "debug, info, warn or error")
	flag.IntVar(&cfg.keepActions, "keep-actions", 5000, "action log entries to keep, 0 to keep everything")
	flag.BoolVar(&cfg.listDevices, "list-devices", false, "list the detected keyboards and exit")

	flag.Parse()
	return cfg
}

func newLogger(level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", level)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})), nil
}

// newUploader builds the lemmary uploader when both a URL and an API key are
// configured. The key can come from a file (--lemmary-api-key-file), so it
// need not be passed on the command line where it would be world-readable
// via /proc or process listings. Uploading is optional, so a nil
// session.Uploader is returned when unconfigured, which disables the
// feature entirely.
func newUploader(cfg config) (session.Uploader, error) {
	if cfg.devLemmary {
		return &lemmary.DevUploader{}, nil
	}

	apiKey := cfg.lemmaryAPIKey
	if cfg.lemmaryAPIKeyFile != "" {
		content, err := os.ReadFile(cfg.lemmaryAPIKeyFile)
		if err != nil {
			return nil, fmt.Errorf("read lemmary API key: %w", err)
		}
		apiKey = strings.TrimSpace(string(content))
	}

	if cfg.lemmaryURL == "" || apiKey == "" {
		return nil, nil
	}
	return &lemmary.Client{BaseURL: cfg.lemmaryURL, APIKey: apiKey}, nil
}

// newScanner builds the page scanner. --scan-command opts into an external
// script instead; otherwise scantool scans pages itself via the SANE,
// imagemagick and img2pdf command line tools, configured through the
// SCAN_DEVICE, SCAN_RESOLUTION, SCAN_MODE and SCAN_SOURCE environment
// variables (the same ones the former scan-page.sh script read).
func newScanner(cfg config) scan.Scanner {
	if cfg.devScanner {
		return &scan.DevScanner{}
	}
	if cfg.scanCmd != "" {
		return &scan.ShellScanner{Command: cfg.scanCmd, Timeout: cfg.scanTimeout}
	}
	return &scan.SaneScanner{
		Device:     os.Getenv("SCAN_DEVICE"),
		Resolution: os.Getenv("SCAN_RESOLUTION"),
		Mode:       os.Getenv("SCAN_MODE"),
		Source:     os.Getenv("SCAN_SOURCE"),
		Timeout:    cfg.scanTimeout,
	}
}

func newMerger(command string) pdfmerge.Merger {
	if command == "" {
		return &pdfmerge.PDFCPU{}
	}
	fields := strings.Fields(command)
	return &pdfmerge.Command{Name: fields[0], Args: fields[1:]}
}

func newKeySource(cfg config, logger *slog.Logger) (keys.Source, error) {
	var paths []string
	for _, p := range strings.Split(cfg.devices, ",") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}

	switch cfg.input {
	case "evdev":
		grab, err := resolveGrab(cfg.grab, len(paths) > 0)
		if err != nil {
			return nil, err
		}
		if grab && len(paths) == 0 {
			logger.Warn("grabbing every detected keyboard, nothing else on this machine will see key presses")
		}
		return &keys.Evdev{
			Paths:       paths,
			Grab:        grab,
			RequireKeys: session.BoundKeys(),
			Logger:      logger,
		}, nil
	case "libinput":
		return &keys.Libinput{Paths: paths, Logger: logger}, nil
	case "stdin":
		return keys.NewStdin(), nil
	default:
		return nil, fmt.Errorf("unknown input source %q, want evdev, libinput or stdin", cfg.input)
	}
}

// resolveGrab decides whether to take exclusive control of the input devices.
//
// "auto" only grabs devices that were named with --device. Grabbing every
// auto-detected keyboard would take the machine's own keyboard away from
// whoever is using it, which is rarely what someone wants while trying the
// daemon out.
func resolveGrab(mode string, explicitDevices bool) (bool, error) {
	switch mode {
	case "auto", "":
		return explicitDevices, nil
	case "yes", "true":
		return true, nil
	case "no", "false":
		return false, nil
	default:
		return false, fmt.Errorf("invalid --grab %q, want auto, yes or no", mode)
	}
}

func listDevices() error {
	devices, err := keys.Keyboards()
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		fmt.Println("no keyboards found")
		return nil
	}

	bound := session.BoundKeys()
	for _, d := range devices {
		fmt.Printf("%-20s %-40s %s\n", d.Path, d.Name, describeDevice(d, bound))
	}
	return nil
}

// describeDevice says whether a device is one the daemon would listen to.
func describeDevice(d keys.Device, bound []rune) string {
	usable, err := keys.CanProduce(d.Path, bound)
	switch {
	case os.IsPermission(err):
		return "no access, needs the input group"
	case err != nil:
		return "unusable: " + err.Error()
	case usable:
		return "usable"
	default:
		return "ignored, cannot send " + string(bound)
	}
}
