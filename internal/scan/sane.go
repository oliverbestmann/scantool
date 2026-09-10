package scan

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// SaneScanner scans a page via SANE's scanimage and turns the raw PNM output
// into a single page PDF using imagemagick and img2pdf. It is the built-in
// replacement for the former scan-page.sh shell script and expects the same
// tools (scanimage, magick, img2pdf) on PATH.
type SaneScanner struct {
	// Device is passed as scanimage's --device-name. Empty uses scanimage's
	// default device.
	Device string
	// Resolution is passed as scanimage's --resolution, in dpi. Defaults to
	// "300".
	Resolution string
	// Mode is passed as scanimage's --mode, e.g. "Color", "Gray" or
	// "Lineart". Defaults to "Color".
	Mode string
	// Source is passed as scanimage's --source, e.g. "Flatbed" or "ADF".
	// Empty uses the scanner's default source.
	Source string

	// ScanimageCmd, MagickCmd and Img2pdfCmd name the executables to run.
	// They default to "scanimage", "magick" and "img2pdf", resolved via
	// PATH.
	ScanimageCmd string
	MagickCmd    string
	Img2pdfCmd   string

	// Timeout aborts a scan that takes too long. Zero means no timeout.
	Timeout time.Duration
}

// ScanPage scans one page and writes a single page PDF to req.Dest.
func (s *SaneScanner) ScanPage(ctx context.Context, req Request) error {
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}

	tmp, err := os.MkdirTemp("", "scantool-scan-*")
	if err != nil {
		return fmt.Errorf("scan: create temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	pnm := filepath.Join(tmp, "page.pnm")
	if err := s.scanImage(ctx, pnm); err != nil {
		return err
	}

	jpg := pnm + ".jpg"
	if err := s.runTool(ctx, s.cmd(s.MagickCmd, "magick"),
		"convert", "-quality", "95", "-level", "0%,90%", pnm, jpg); err != nil {
		return fmt.Errorf("scan: convert: %w", err)
	}

	if err := s.runTool(ctx, s.cmd(s.Img2pdfCmd, "img2pdf"), "--output", req.Dest, jpg); err != nil {
		os.Remove(req.Dest)
		return fmt.Errorf("scan: img2pdf: %w", err)
	}

	info, err := os.Stat(req.Dest)
	if err != nil {
		return fmt.Errorf("scan: img2pdf did not create %s", req.Dest)
	}
	if info.Size() == 0 {
		os.Remove(req.Dest)
		return fmt.Errorf("scan: img2pdf produced an empty file")
	}

	return nil
}

// scanImage runs scanimage, writing its raw PNM output to dest.
func (s *SaneScanner) scanImage(ctx context.Context, dest string) error {
	resolution := s.Resolution
	if resolution == "" {
		resolution = "300"
	}
	mode := s.Mode
	if mode == "" {
		mode = "Color"
	}

	args := []string{"--format=pnm", "--resolution", resolution, "--mode", mode}
	if s.Device != "" {
		args = append(args, "--device-name", s.Device)
	}
	if s.Source != "" {
		args = append(args, "--source", s.Source)
	}

	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("scan: create %s: %w", dest, err)
	}
	defer out.Close()

	cmd := exec.CommandContext(ctx, s.cmd(s.ScanimageCmd, "scanimage"), args...)
	setupProcessGroup(cmd)
	cmd.Stdout = out

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		os.Remove(dest)
		if ctx.Err() != nil {
			return fmt.Errorf("scan: scanimage: %w%s", ctx.Err(), tail(stderr.String()))
		}
		return fmt.Errorf("scan: scanimage: %w%s", err, tail(stderr.String()))
	}

	info, err := os.Stat(dest)
	if err != nil || info.Size() == 0 {
		os.Remove(dest)
		return fmt.Errorf("scan: scanimage produced an empty image%s", tail(stderr.String()))
	}
	return nil
}

// runTool runs one of the conversion tools, capturing combined output for
// error messages.
func (s *SaneScanner) runTool(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	setupProcessGroup(cmd)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w%s", name, ctx.Err(), tail(out.String()))
		}
		return fmt.Errorf("%s: %w%s", name, err, tail(out.String()))
	}
	return nil
}

func (s *SaneScanner) cmd(configured, fallback string) string {
	if configured != "" {
		return configured
	}
	return fallback
}
