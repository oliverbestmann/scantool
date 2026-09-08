// Package scan produces single page PDFs from a scanner.
package scan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Request describes the page that should be scanned.
type Request struct {
	// Dest is the file the scanner must write a single page PDF to.
	Dest string
	// SessionID is the session the page belongs to, for logging/naming.
	SessionID int64
	// Page is the 1-based page number within the session.
	Page int
}

// Scanner scans a single page.
type Scanner interface {
	ScanPage(ctx context.Context, req Request) error
}

// Func adapts a plain function to the Scanner interface.
type Func func(ctx context.Context, req Request) error

func (f Func) ScanPage(ctx context.Context, req Request) error { return f(ctx, req) }

// ShellScanner shells out to a script (scan-page.sh by default).
//
// The script is invoked as
//
//	scan-page.sh <destination.pdf>
//
// and must write a single page PDF to that path. The environment additionally
// carries SCANTOOL_DEST, SCANTOOL_SESSION and SCANTOOL_PAGE.
type ShellScanner struct {
	// Command is the script or binary to run.
	Command string
	// Args are passed before the destination path.
	Args []string
	// Timeout aborts a scan that takes too long. Zero means no timeout.
	Timeout time.Duration
	// Dir is the working directory for the command. Empty means inherit.
	Dir string
}

// ScanPage runs the configured command and checks that it produced a file.
func (s *ShellScanner) ScanPage(ctx context.Context, req Request) error {
	if s.Command == "" {
		return errors.New("scan: no command configured")
	}

	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}

	args := append(append([]string(nil), s.Args...), req.Dest)

	cmd := exec.CommandContext(ctx, s.Command, args...)
	setupProcessGroup(cmd)
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(),
		"SCANTOOL_DEST="+req.Dest,
		"SCANTOOL_SESSION="+strconv.FormatInt(req.SessionID, 10),
		"SCANTOOL_PAGE="+strconv.Itoa(req.Page),
	)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		// Clean up a partially written page so it never ends up in the PDF.
		os.Remove(req.Dest)
		if ctx.Err() != nil {
			return fmt.Errorf("scan: %s: %w%s", s.Command, ctx.Err(), tail(out.String()))
		}
		return fmt.Errorf("scan: %s: %w%s", s.Command, err, tail(out.String()))
	}

	info, err := os.Stat(req.Dest)
	if err != nil {
		return fmt.Errorf("scan: %s did not create %s%s", s.Command, req.Dest, tail(out.String()))
	}
	if info.Size() == 0 {
		os.Remove(req.Dest)
		return fmt.Errorf("scan: %s produced an empty file%s", s.Command, tail(out.String()))
	}

	return nil
}

// tail returns the last few lines of command output, formatted for appending
// to an error message.
func tail(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	lines := strings.Split(s, "\n")
	const max = 5
	if len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	return ": " + strings.Join(lines, " | ")
}
