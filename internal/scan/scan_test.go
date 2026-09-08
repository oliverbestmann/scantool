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

// writeScript drops a stand-in for scan-page.sh into dir. There is no SANE on
// the build machine, so every test drives the scanner through a shell script.
func writeScript(t *testing.T, dir, body string) string {
	t.Helper()

	path := filepath.Join(dir, "scan-page.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestShellScannerWritesPage(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `echo "%PDF-1.4 fake page" > "$1"`)

	dest := filepath.Join(dir, "page-001.pdf")
	scanner := &scan.ShellScanner{Command: script}

	err := scanner.ScanPage(t.Context(), scan.Request{Dest: dest, SessionID: 4, Page: 1})
	if err != nil {
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

func TestShellScannerPassesEnvironment(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `printf '%s|%s|%s|%s' "$1" "$SCANTOOL_DEST" "$SCANTOOL_SESSION" "$SCANTOOL_PAGE" > "$1"`)

	dest := filepath.Join(dir, "page.pdf")
	scanner := &scan.ShellScanner{Command: script}

	if err := scanner.ScanPage(t.Context(), scan.Request{Dest: dest, SessionID: 12, Page: 3}); err != nil {
		t.Fatalf("ScanPage: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	want := dest + "|" + dest + "|12|3"
	if string(got) != want {
		t.Fatalf("script saw %q, want %q", got, want)
	}
}

func TestShellScannerPassesExtraArgs(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `printf '%s' "$*" > "$3"`)

	dest := filepath.Join(dir, "page.pdf")
	scanner := &scan.ShellScanner{Command: script, Args: []string{"--device", "epson"}}

	if err := scanner.ScanPage(t.Context(), scan.Request{Dest: dest}); err != nil {
		t.Fatalf("ScanPage: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if want := "--device epson " + dest; string(got) != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestShellScannerReportsFailure(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
echo "scanimage: no SANE devices found" >&2
exit 1
`)

	dest := filepath.Join(dir, "page.pdf")
	err := (&scan.ShellScanner{Command: script}).ScanPage(t.Context(), scan.Request{Dest: dest})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "no SANE devices found") {
		t.Fatalf("error %q does not mention the script output", err)
	}
}

func TestShellScannerRemovesPartialPage(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
echo "half a page" > "$1"
exit 1
`)

	dest := filepath.Join(dir, "page.pdf")
	if err := (&scan.ShellScanner{Command: script}).ScanPage(t.Context(), scan.Request{Dest: dest}); err == nil {
		t.Fatal("want error, got nil")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("a failed scan must not leave a partial page behind")
	}
}

func TestShellScannerRequiresOutputFile(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `echo "pretending to scan"`)

	dest := filepath.Join(dir, "page.pdf")
	err := (&scan.ShellScanner{Command: script}).ScanPage(t.Context(), scan.Request{Dest: dest})
	if err == nil {
		t.Fatal("want error when the script writes no file, got nil")
	}
	if !strings.Contains(err.Error(), "did not create") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestShellScannerRejectsEmptyPage(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `: > "$1"`)

	dest := filepath.Join(dir, "page.pdf")
	err := (&scan.ShellScanner{Command: script}).ScanPage(t.Context(), scan.Request{Dest: dest})
	if err == nil {
		t.Fatal("want error for an empty page, got nil")
	}
	if !strings.Contains(err.Error(), "empty file") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("the empty page should have been removed")
	}
}

func TestShellScannerTimesOut(t *testing.T) {
	dir := t.TempDir()
	// The backgrounded sleep inherits the output pipe: unless the whole
	// process group is cancelled, waiting for the command would hang long
	// after the script itself is gone.
	script := writeScript(t, dir, `
sleep 30 &
sleep 30
`)

	dest := filepath.Join(dir, "page.pdf")
	scanner := &scan.ShellScanner{Command: script, Timeout: 100 * time.Millisecond}

	start := time.Now()
	err := scanner.ScanPage(t.Context(), scan.Request{Dest: dest})
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

func TestShellScannerHonoursCancellation(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `sleep 30`)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := (&scan.ShellScanner{Command: script}).ScanPage(ctx, scan.Request{Dest: filepath.Join(dir, "page.pdf")})
	if err == nil {
		t.Fatal("want error after cancellation, got nil")
	}
}

func TestShellScannerNeedsCommand(t *testing.T) {
	err := (&scan.ShellScanner{}).ScanPage(t.Context(), scan.Request{Dest: "page.pdf"})
	if err == nil {
		t.Fatal("want error without a command, got nil")
	}
}

func TestFuncAdapter(t *testing.T) {
	var got scan.Request

	var scanner scan.Scanner = scan.Func(func(_ context.Context, req scan.Request) error {
		got = req
		return nil
	})

	want := scan.Request{Dest: "x.pdf", SessionID: 1, Page: 2}
	if err := scanner.ScanPage(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("request = %+v, want %+v", got, want)
	}
}
