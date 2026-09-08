package keys

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// DefaultLibinputCommand is the tool used by the libinput backend.
var DefaultLibinputCommand = "libinput"

// Libinput reads key presses from "libinput debug-events". It is an
// alternative to Evdev for machines where libinput is already set up. Note
// that libinput hides the keycodes ("*** (-1)") unless --show-keycodes is
// passed, which this backend always does, and that it needs the same
// privileges as reading the device directly.
type Libinput struct {
	// Command is the libinput binary. Defaults to DefaultLibinputCommand.
	Command string
	// Paths are the devices to watch. Empty watches every device libinput
	// knows about.
	Paths []string
	// Logger receives diagnostics. Defaults to slog.Default().
	Logger *slog.Logger
}

// Name implements Source.
func (l *Libinput) Name() string {
	if len(l.Paths) == 0 {
		return "libinput (all devices)"
	}
	return "libinput " + strings.Join(l.Paths, ",")
}

// Run implements Source.
func (l *Libinput) Run(ctx context.Context, out chan<- rune) error {
	log := l.Logger
	if log == nil {
		log = slog.Default()
	}

	name := l.Command
	if name == "" {
		name = DefaultLibinputCommand
	}

	args := []string{"debug-events", "--show-keycodes"}
	for _, p := range l.Paths {
		args = append(args, "--device", p)
	}

	cmd := exec.CommandContext(ctx, name, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("keys: libinput stdout: %w", err)
	}

	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("keys: start %s: %w", name, err)
	}

	log.Info("reading key presses from libinput", "command", name, "args", strings.Join(args, " "))

	// Killing the process is what stops the scanner below.
	var once sync.Once
	stopKill := context.AfterFunc(ctx, func() {
		once.Do(func() {
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
		})
	})
	defer stopKill()

	scanErr := ScanLibinput(ctx, stdout, func(key rune) bool {
		return send(ctx, out, key)
	}, log)

	// Drain anything left so libinput does not block on a full pipe.
	io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		return nil
	}
	if scanErr != nil {
		return scanErr
	}
	if waitErr != nil {
		return fmt.Errorf("keys: %s exited: %w: %s", name, waitErr, strings.TrimSpace(stderr.String()))
	}
	return errors.New("keys: libinput exited unexpectedly")
}

// ScanLibinput parses the output of "libinput debug-events --show-keycodes"
// and calls onKey for every key press. Scanning stops when onKey returns
// false, when r ends, or when ctx is cancelled.
func ScanLibinput(ctx context.Context, r io.Reader, onKey func(rune) bool, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}

	var warnedMasked bool

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return nil
		}

		line := scanner.Text()
		key, state, ok := ParseLibinputEvent(line)
		if !ok {
			if !warnedMasked && strings.Contains(line, "KEYBOARD_KEY") && strings.Contains(line, "***") {
				// This is what libinput prints without --show-keycodes.
				log.Warn("libinput is hiding keycodes, cannot read keys from it", "line", strings.TrimSpace(line))
				warnedMasked = true
			}
			continue
		}
		// Acting on the press keeps the behaviour identical to the evdev
		// backend; the matching release is ignored.
		if state != "pressed" {
			continue
		}
		if !onKey(key) {
			return nil
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("keys: read libinput output: %w", err)
	}
	return nil
}

// ParseLibinputEvent extracts the key and its state from a KEYBOARD_KEY line
// such as
//
//	event19  KEYBOARD_KEY  +0.000s	KEY_A (30) pressed
//
// It reports false for other lines, for keys this daemon does not map, and
// for the masked "*** (-1)" form libinput prints without --show-keycodes.
func ParseLibinputEvent(line string) (key rune, state string, ok bool) {
	if !strings.Contains(line, "KEYBOARD_KEY") {
		return 0, "", false
	}

	fields := strings.Fields(line)
	for i, f := range fields {
		if f != "pressed" && f != "released" {
			continue
		}
		if i < 2 {
			return 0, "", false
		}

		// fields[i-2] is the KEY_* name, fields[i-1] the "(30)" keycode.
		if key, found := RuneForName(fields[i-2]); found {
			return key, f, true
		}

		code := strings.Trim(fields[i-1], "()")
		if n, err := strconv.Atoi(code); err == nil && n >= 0 {
			if key, found := RuneForCode(uint16(n)); found {
				return key, f, true
			}
		}
		return 0, "", false
	}

	return 0, "", false
}
