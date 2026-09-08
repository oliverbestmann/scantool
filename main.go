// Command scantool is a small daemon for a headless Raspberry Pi: it listens
// for key presses on a USB keypad and turns them into scanned PDF documents.
//
//	a  start a new document and scan its first page
//	b  scan another page into the current document
//	c  finish the document and store it as <timestamp>.pdf
//
// Pressing "a" while a document already holds pages stores that document
// first. Pressing "b" without an open document starts one.
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
	mergeCmd    string
	fileLayout  string

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

	manager, err := session.New(session.Options{
		OutDir:     cfg.outDir,
		WorkDir:    cfg.workDir,
		FileLayout: cfg.fileLayout,
		Scanner:    &scan.ShellScanner{Command: cfg.scanCmd, Timeout: cfg.scanTimeout},
		Merger:     newMerger(cfg.mergeCmd),
		Recorder:   db,
		Logger:     logger,
	})
	if err != nil {
		return err
	}

	source, err := newKeySource(cfg, logger)
	if err != nil {
		return err
	}

	// Signals stop the daemon; an open document is stored on the way out.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Key presses and web requests both feed the single action loop, so only
	// one scan runs at a time and presses during a scan are queued.
	keyCh := make(chan rune, 32)
	actionCh := make(chan session.Action, 32)

	var submit func(session.Action) error
	if cfg.webControl {
		submit = func(a session.Action) error {
			select {
			case actionCh <- a:
				manager.Record(store.Action{Kind: store.KindWebAction, Detail: string(a)})
				return nil
			default:
				return errors.New("scantool is busy, try again in a moment")
			}
		}
	}

	if cfg.httpAddr != "" {
		server, err := web.New(web.Options{
			Addr:   cfg.httpAddr,
			State:  manager.State,
			Reader: db,
			Submit: submit,
			Logger: logger,
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

	logger.Info("scantool started",
		"input", source.Name(),
		"out", cfg.outDir,
		"scan_command", cfg.scanCmd,
		"http", cfg.httpAddr)
	logger.Info("key bindings: a = new document, b = add page, c = finish document")

	manager.Record(store.Action{Kind: store.KindDaemonStarted, Detail: source.Name()})

	err = loop(ctx, manager, keyCh, actionCh, sourceDone, logger)

	// Never lose pages that were already scanned.
	if closeErr := manager.Close(); closeErr != nil {
		logger.Error("could not store open document on shutdown", "error", closeErr)
	}
	manager.Record(store.Action{Kind: store.KindDaemonStopped})
	logger.Info("scantool stopped")

	return err
}

// loop runs actions one at a time until the context is cancelled or the key
// source gives up.
func loop(ctx context.Context, manager *session.Manager, keyCh <-chan rune, actionCh <-chan session.Action, sourceDone <-chan error, logger *slog.Logger) error {
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
			// Errors are already logged and recorded by the manager; the
			// daemon keeps running so the next key press still works.
			if err := manager.HandleKey(ctx, key); err != nil {
				logger.Debug("action failed", "error", err)
			}

		case action := <-actionCh:
			if err := manager.Do(ctx, action); err != nil {
				logger.Debug("action failed", "error", err)
			}
		}
	}
}

func parseFlags() config {
	var cfg config

	flag.StringVar(&cfg.outDir, "out", "scans", "directory for the finished PDF documents")
	flag.StringVar(&cfg.workDir, "work", "", "directory for pages of open documents (default <out>/.scantool-work)")
	flag.StringVar(&cfg.dbPath, "db", "", "SQLite database for sessions and the action log (default <out>/scantool.db)")
	flag.StringVar(&cfg.scanCmd, "scan-command", "./scan-page.sh", "command scanning one page, called as <command> <output.pdf>")
	flag.DurationVar(&cfg.scanTimeout, "scan-timeout", 3*time.Minute, "abort a scan that takes longer than this")
	flag.StringVar(&cfg.mergeCmd, "merge-command", "", "external merge command, e.g. \"pdfunite {{in}} {{out}}\" (default: built-in pdfcpu)")
	flag.StringVar(&cfg.fileLayout, "name-layout", "20060102-150405", "Go time layout for the output file name")

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
