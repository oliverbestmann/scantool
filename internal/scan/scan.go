// Package scan produces single page PDFs from a scanner.
package scan

import (
	"context"
	"strings"
)

// Request describes the page that should be scanned.
type Request struct {
	// Dest is the file the scanner must write a single page PDF to.
	Dest string
	// SessionID is the session the page belongs to, for logging/naming.
	SessionID int64
	// Page is the 1-based page number within the session.
	Page int
	// Resolution overrides the scanner's configured resolution, in dpi.
	// Empty uses the scanner's own default.
	Resolution string
	// ThumbDest, if set, is the file the scanner must write a small JPEG
	// thumbnail of the page to, besides the PDF at Dest.
	ThumbDest string
}

// thumbnailWidth is the width, in pixels, of the thumbnail JPEG scanners
// write alongside the page when Request.ThumbDest is set. Height follows
// from the page's aspect ratio.
const thumbnailWidth = 200

// Scanner scans a single page.
type Scanner interface {
	ScanPage(ctx context.Context, req Request) error
}

// Func adapts a plain function to the Scanner interface.
type Func func(ctx context.Context, req Request) error

func (f Func) ScanPage(ctx context.Context, req Request) error { return f(ctx, req) }

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
