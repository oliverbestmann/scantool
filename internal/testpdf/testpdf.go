// Package testpdf creates small but real PDF files for tests, so the tests do
// not need a scanner, SANE or any fixture files checked into the repository.
package testpdf

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
)

// pdfcpu otherwise reads/writes a per-user config directory on first use;
// disable it so tests don't depend on it being accessible.
func init() {
	api.DisableConfigDir()
}

// Write creates a valid single page PDF at path. The page is a small solid
// colour image, which is enough for merging and page counting.
func Write(t *testing.T, path string, shade uint8) {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{R: shade, G: shade, B: shade, A: 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}

	imgPath := filepath.Join(t.TempDir(), "page.png")
	if err := os.WriteFile(imgPath, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write png: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create directory: %v", err)
	}
	if err := api.ImportImagesFile([]string{imgPath}, path, nil, nil); err != nil {
		t.Fatalf("create pdf %s: %v", path, err)
	}
}

// PageCount returns the number of pages of a PDF, failing the test if the
// file cannot be read.
func PageCount(t *testing.T, path string) int {
	t.Helper()

	count, err := api.PageCountFile(path)
	if err != nil {
		t.Fatalf("page count of %s: %v", path, err)
	}
	return count
}

// Validate fails the test if path is not a readable, valid PDF.
func Validate(t *testing.T, path string) {
	t.Helper()

	if err := api.ValidateFile(path, nil); err != nil {
		t.Fatalf("validate %s: %v", path, err)
	}
}
