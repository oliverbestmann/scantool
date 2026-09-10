package scan

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
)

// pdfcpu otherwise reads/writes a per-user config directory (and its fonts)
// on first use, which a headless daemon may not have access to. Disable it;
// scantool doesn't need user fonts.
func init() {
	api.DisableConfigDir()
}

// DevScanner fakes a scan without any hardware: it waits Delay, simulating
// how long a real scan takes, then writes a small blank single-page PDF to
// req.Dest. It's meant for developing and testing scantool without a
// scanner attached.
type DevScanner struct {
	// Delay simulates how long a real scan takes. Zero means 3s.
	Delay time.Duration
}

// ScanPage waits and then writes a blank page PDF to req.Dest.
func (s *DevScanner) ScanPage(ctx context.Context, req Request) error {
	delay := s.Delay
	if delay <= 0 {
		delay = 3 * time.Second
	}

	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return ctx.Err()
	}

	if req.ThumbDest != "" {
		if err := writeBlankThumbnail(req.ThumbDest); err != nil {
			return err
		}
	}

	return writeBlankPDF(req.Dest)
}

// writeBlankThumbnail writes a small blank JPEG to dest, standing in for the
// real scanner's resized page.
func writeBlankThumbnail(dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("scan: dev: create thumbnail directory: %w", err)
	}

	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("scan: dev: create thumbnail: %w", err)
	}
	defer f.Close()

	img := image.NewRGBA(image.Rect(0, 0, thumbnailWidth, thumbnailWidth*141/100))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 85}); err != nil {
		return fmt.Errorf("scan: dev: encode thumbnail: %w", err)
	}
	return f.Close()
}

// writeBlankPDF creates a valid single-page PDF at dest, showing a plain
// white page. pdfcpu needs an image to import, so a 1x1 PNG is generated in
// a temporary file first.
func writeBlankPDF(dest string) error {
	imgFile, err := os.CreateTemp("", "scantool-dev-*.png")
	if err != nil {
		return fmt.Errorf("scan: dev: create temp image: %w", err)
	}
	defer os.Remove(imgFile.Name())
	defer imgFile.Close()

	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.White)
	if err := png.Encode(imgFile, img); err != nil {
		return fmt.Errorf("scan: dev: encode blank page: %w", err)
	}
	if err := imgFile.Close(); err != nil {
		return fmt.Errorf("scan: dev: write blank page: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("scan: dev: create destination directory: %w", err)
	}
	if err := api.ImportImagesFile([]string{imgFile.Name()}, dest, nil, nil); err != nil {
		return fmt.Errorf("scan: dev: create pdf: %w", err)
	}

	return nil
}
