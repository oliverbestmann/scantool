package scan

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	// Width is passed as scanimage's -x, the scan area width in mm.
	// Defaults to "210" (A4).
	Width string
	// Height is passed as scanimage's -y, the scan area height in mm.
	// Defaults to "297" (A4).
	Height string

	// ScanimageCmd, MagickCmd and Img2pdfCmd name the executables to run.
	// They default to "scanimage", "magick" and "img2pdf", resolved via
	// PATH.
	ScanimageCmd string
	MagickCmd    string
	Img2pdfCmd   string

	// Timeout aborts a scan that takes too long. Zero means no timeout.
	Timeout time.Duration
}

// ScanPage scans one page and writes a single page PDF to req.Dest. It is
// AcquireImage followed by ProcessImage; callers that want the scanner
// hardware freed up for the next page while this one converts should call
// the two separately instead (see TwoPhaseScanner).
func (s *SaneScanner) ScanPage(ctx context.Context, req Request) error {
	image, err := s.AcquireImage(ctx, req)
	if err != nil {
		return err
	}
	return s.ProcessImage(ctx, req, image)
}

// AcquireImage is the hardware-bound half of ScanPage: it runs scanimage and
// returns its raw PNM output. Calls to AcquireImage must not overlap with
// each other, since they drive the same physical scanner; ProcessImage may
// run concurrently with the next AcquireImage.
func (s *SaneScanner) AcquireImage(ctx context.Context, req Request) ([]byte, error) {
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	return s.scanImage(ctx, req.Resolution)
}

// ProcessImage is the CPU-bound half of ScanPage: it turns a PNM image
// previously returned by AcquireImage into a single page PDF at req.Dest.
// Safe to run concurrently with the next page's AcquireImage.
func (s *SaneScanner) ProcessImage(ctx context.Context, req Request, image []byte) error {
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

	jpg := filepath.Join(tmp, "page.jpg")
	if err := s.runTool(ctx, s.cmd(s.MagickCmd, "magick"), bytes.NewReader(image),
		"convert", "-quality", "95", "-level", "0%,90%", "pnm:-", jpg); err != nil {
		return fmt.Errorf("scan: convert: %w", err)
	}

	if req.ThumbDest != "" {
		if err := s.runTool(ctx, s.cmd(s.MagickCmd, "magick"), nil,
			"convert", jpg, "-resize", fmt.Sprintf("%dx", thumbnailWidth), "-quality", "85", req.ThumbDest); err != nil {
			return fmt.Errorf("scan: thumbnail: %w", err)
		}
	}

	if err := s.runTool(ctx, s.cmd(s.Img2pdfCmd, "img2pdf"), nil, "--output", req.Dest, jpg); err != nil {
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

// scanImage runs scanimage and returns its raw PNM output, read straight
// into memory rather than through a temp file. The buffer is pre-sized from
// the expected image dimensions so it rarely needs to grow. reqResolution
// overrides the scanner's configured Resolution when set, e.g. per-request
// from the web UI.
func (s *SaneScanner) scanImage(ctx context.Context, reqResolution string) ([]byte, error) {
	resolution := s.resolution()
	if reqResolution != "" {
		resolution = reqResolution
	}

	args := []string{
		"--format=pnm", "--resolution", resolution, "--mode", s.mode(),
		"-x", s.width(), "-y", s.height(),
	}
	if s.Device != "" {
		args = append(args, "--device-name", s.Device)
	}
	if s.Source != "" {
		args = append(args, "--source", s.Source)
	}

	cmd := exec.CommandContext(ctx, s.cmd(s.ScanimageCmd, "scanimage"), args...)
	setupProcessGroup(cmd)

	var stdout bytes.Buffer
	stdout.Grow(s.estimatePNMSize())
	cmd.Stdout = &stdout

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("scan: scanimage: %w%s", ctx.Err(), tail(stderr.String()))
		}
		return nil, fmt.Errorf("scan: scanimage: %w%s", err, tail(stderr.String()))
	}

	if stdout.Len() == 0 {
		return nil, fmt.Errorf("scan: scanimage produced an empty image%s", tail(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// runTool runs one of the conversion tools, capturing combined output for
// error messages. A nil stdin leaves the tool's stdin untouched.
func (s *SaneScanner) runTool(ctx context.Context, name string, stdin io.Reader, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	setupProcessGroup(cmd)
	cmd.Stdin = stdin

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

func (s *SaneScanner) resolution() string { return orDefault(s.Resolution, "300") }
func (s *SaneScanner) mode() string       { return orDefault(s.Mode, "Color") }
func (s *SaneScanner) width() string      { return orDefault(s.Width, "210") }
func (s *SaneScanner) height() string     { return orDefault(s.Height, "297") }

func orDefault(configured, fallback string) string {
	if configured != "" {
		return configured
	}
	return fallback
}

// estimatePNMSize predicts the size of scanimage's PNM output from the
// configured width, height and resolution, so the buffer that receives it
// can be preallocated instead of growing one reallocation at a time. It pads
// the estimate by 10% plus a small constant for the PNM header.
func (s *SaneScanner) estimatePNMSize() int {
	widthMM, _ := strconv.ParseFloat(s.width(), 64)
	heightMM, _ := strconv.ParseFloat(s.height(), 64)
	dpi, _ := strconv.ParseFloat(s.resolution(), 64)

	widthPx := widthMM / 25.4 * dpi
	heightPx := heightMM / 25.4 * dpi

	var bytesPerPixel float64
	switch s.mode() {
	case "Gray":
		bytesPerPixel = 1
	case "Lineart":
		bytesPerPixel = 1.0 / 8
	default: // Color
		bytesPerPixel = 3
	}

	size := widthPx * heightPx * bytesPerPixel
	return int(size*1.1) + 64
}
