package scan_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oliverbestmann/scantool/internal/scan"
)

// writeTool drops a stand-in for a command line tool into dir and returns
// its path. There is no SANE/imagemagick/img2pdf on the build machine, so
// every test drives SaneScanner through fake tools.
func writeTool(t *testing.T, dir, name, body string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// happyScanner returns a SaneScanner whose three tools cooperate: scanimage
// writes a non-empty PNM to stdout, magick "converts" it by copying, and
// img2pdf writes a fake PDF to its --output path.
func happyScanner(t *testing.T) *scan.SaneScanner {
	t.Helper()
	dir := t.TempDir()

	scanimage := writeTool(t, dir, "scanimage", `echo "P6 fake pnm" `)
	magick := writeTool(t, dir, "magick", `
# args: convert -quality 95 -level 0%,90% <in> <out>
cp "$6" "$7"
`)
	img2pdf := writeTool(t, dir, "img2pdf", `
# args: --output <dest> <in>
echo "%PDF-1.4 fake page" > "$2"
`)

	return &scan.SaneScanner{
		ScanimageCmd: scanimage,
		MagickCmd:    magick,
		Img2pdfCmd:   img2pdf,
	}
}

func TestSaneScannerWritesPage(t *testing.T) {
	s := happyScanner(t)

	dest := filepath.Join(t.TempDir(), "page-001.pdf")
	if err := s.ScanPage(t.Context(), scan.Request{Dest: dest, SessionID: 4, Page: 1}); err != nil {
		t.Fatalf("ScanPage: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("scanned page missing: %v", err)
	}
	if !strings.HasPrefix(string(got), "%PDF") {
		t.Fatalf("page content = %q", got)
	}
}

func TestSaneScannerPassesResolutionModeDeviceSource(t *testing.T) {
	dir := t.TempDir()
	scanimage := writeTool(t, dir, "scanimage", `printf '%s' "$*" > "$dir/args"; echo "P6 fake pnm"`)
	magick := writeTool(t, dir, "magick", `cp "$6" "$7"`)
	img2pdf := writeTool(t, dir, "img2pdf", `echo "%PDF-1.4 fake page" > "$2"`)

	s := &scan.SaneScanner{
		ScanimageCmd: scanimage,
		MagickCmd:    magick,
		Img2pdfCmd:   img2pdf,
		Device:       "epson2:libusb:001:002",
		Resolution:   "600",
		Mode:         "Gray",
		Source:       "ADF",
	}

	// The scanimage stand-in cannot see $dir from writeTool's shell, so pass
	// it in explicitly.
	os.Setenv("dir", dir)
	defer os.Unsetenv("dir")

	dest := filepath.Join(dir, "page.pdf")
	if err := s.ScanPage(t.Context(), scan.Request{Dest: dest}); err != nil {
		t.Fatalf("ScanPage: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	args := string(got)
	for _, want := range []string{"--format=pnm", "--resolution 600", "--mode Gray", "--device-name epson2:libusb:001:002", "--source ADF"} {
		if !strings.Contains(args, want) {
			t.Fatalf("args = %q, want it to contain %q", args, want)
		}
	}
}

func TestSaneScannerDefaultsResolutionAndMode(t *testing.T) {
	dir := t.TempDir()
	scanimage := writeTool(t, dir, "scanimage", `printf '%s' "$*" > "$dir/args"; echo "P6 fake pnm"`)
	magick := writeTool(t, dir, "magick", `cp "$6" "$7"`)
	img2pdf := writeTool(t, dir, "img2pdf", `echo "%PDF-1.4 fake page" > "$2"`)

	os.Setenv("dir", dir)
	defer os.Unsetenv("dir")

	s := &scan.SaneScanner{ScanimageCmd: scanimage, MagickCmd: magick, Img2pdfCmd: img2pdf}

	dest := filepath.Join(dir, "page.pdf")
	if err := s.ScanPage(t.Context(), scan.Request{Dest: dest}); err != nil {
		t.Fatalf("ScanPage: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	args := string(got)
	if !strings.Contains(args, "--resolution 300") || !strings.Contains(args, "--mode Color") {
		t.Fatalf("args = %q, want default resolution 300 and mode Color", args)
	}
	if strings.Contains(args, "--device-name") || strings.Contains(args, "--source") {
		t.Fatalf("args = %q, want no --device-name/--source when unset", args)
	}
}

func TestSaneScannerRejectsEmptyScanimageOutput(t *testing.T) {
	dir := t.TempDir()
	scanimage := writeTool(t, dir, "scanimage", `: > /dev/null`) // produces nothing on stdout
	magick := writeTool(t, dir, "magick", `cp "$6" "$7"`)
	img2pdf := writeTool(t, dir, "img2pdf", `echo "%PDF-1.4 fake page" > "$2"`)

	s := &scan.SaneScanner{ScanimageCmd: scanimage, MagickCmd: magick, Img2pdfCmd: img2pdf}

	dest := filepath.Join(dir, "page.pdf")
	err := s.ScanPage(t.Context(), scan.Request{Dest: dest})
	if err == nil {
		t.Fatal("want error for an empty scan, got nil")
	}
	if !strings.Contains(err.Error(), "empty image") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("no page should have been written")
	}
}

func TestSaneScannerReportsScanimageFailure(t *testing.T) {
	dir := t.TempDir()
	scanimage := writeTool(t, dir, "scanimage", `
echo "scanimage: no SANE devices found" >&2
exit 1
`)
	magick := writeTool(t, dir, "magick", `cp "$6" "$7"`)
	img2pdf := writeTool(t, dir, "img2pdf", `echo "%PDF-1.4 fake page" > "$2"`)

	s := &scan.SaneScanner{ScanimageCmd: scanimage, MagickCmd: magick, Img2pdfCmd: img2pdf}

	dest := filepath.Join(dir, "page.pdf")
	err := s.ScanPage(t.Context(), scan.Request{Dest: dest})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "no SANE devices found") {
		t.Fatalf("error %q does not mention scanimage's output", err)
	}
}

func TestSaneScannerReportsMagickFailure(t *testing.T) {
	dir := t.TempDir()
	scanimage := writeTool(t, dir, "scanimage", `echo "P6 fake pnm"`)
	magick := writeTool(t, dir, "magick", `echo "magick: unable to open image" >&2; exit 1`)
	img2pdf := writeTool(t, dir, "img2pdf", `echo "%PDF-1.4 fake page" > "$2"`)

	s := &scan.SaneScanner{ScanimageCmd: scanimage, MagickCmd: magick, Img2pdfCmd: img2pdf}

	dest := filepath.Join(dir, "page.pdf")
	err := s.ScanPage(t.Context(), scan.Request{Dest: dest})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "unable to open image") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSaneScannerReportsImg2pdfFailure(t *testing.T) {
	dir := t.TempDir()
	scanimage := writeTool(t, dir, "scanimage", `echo "P6 fake pnm"`)
	magick := writeTool(t, dir, "magick", `cp "$6" "$7"`)
	img2pdf := writeTool(t, dir, "img2pdf", `echo "img2pdf: broken jpeg" >&2; exit 1`)

	s := &scan.SaneScanner{ScanimageCmd: scanimage, MagickCmd: magick, Img2pdfCmd: img2pdf}

	dest := filepath.Join(dir, "page.pdf")
	err := s.ScanPage(t.Context(), scan.Request{Dest: dest})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "broken jpeg") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("a failed img2pdf must not leave a partial page behind")
	}
}

func TestSaneScannerTimesOut(t *testing.T) {
	dir := t.TempDir()
	scanimage := writeTool(t, dir, "scanimage", `
sleep 30 &
sleep 30
`)
	s := &scan.SaneScanner{ScanimageCmd: scanimage, Timeout: 100 * time.Millisecond}

	dest := filepath.Join(dir, "page.pdf")
	start := time.Now()
	err := s.ScanPage(t.Context(), scan.Request{Dest: dest})
	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout took %v, the scan was not aborted", elapsed)
	}
}
