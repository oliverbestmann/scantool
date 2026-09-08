package pdfmerge_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oliverbestmann/scantool/internal/pdfmerge"
	"github.com/oliverbestmann/scantool/internal/testpdf"
)

func TestPDFCPUMergesPages(t *testing.T) {
	dir := t.TempDir()

	var pages []string
	for i, shade := range []uint8{0, 120, 240} {
		page := filepath.Join(dir, fmt.Sprintf("page-%d.pdf", i+1))
		testpdf.Write(t, page, shade)
		pages = append(pages, page)
	}

	dest := filepath.Join(dir, "out", "merged.pdf")

	merger := &pdfmerge.PDFCPU{}
	if err := merger.Merge(pages, dest); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	testpdf.Validate(t, dest)
	if got := testpdf.PageCount(t, dest); got != 3 {
		t.Fatalf("merged page count = %d, want 3", got)
	}
}

func TestPDFCPUSinglePageIsCopiedVerbatim(t *testing.T) {
	dir := t.TempDir()
	page := filepath.Join(dir, "page.pdf")
	testpdf.Write(t, page, 10)

	dest := filepath.Join(dir, "out", "single.pdf")
	if err := (&pdfmerge.PDFCPU{}).Merge([]string{page}, dest); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	want, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("single page document was rewritten, want a verbatim copy")
	}
	if n := testpdf.PageCount(t, dest); n != 1 {
		t.Fatalf("page count = %d, want 1", n)
	}
}

func TestPDFCPURejectsEmptyPageList(t *testing.T) {
	err := (&pdfmerge.PDFCPU{}).Merge(nil, filepath.Join(t.TempDir(), "out.pdf"))
	if err == nil {
		t.Fatal("Merge with no pages: want error, got nil")
	}
}

func TestPDFCPUReportsBrokenInput(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.pdf")
	testpdf.Write(t, good, 50)

	broken := filepath.Join(dir, "broken.pdf")
	if err := os.WriteFile(broken, []byte("this is not a pdf"), 0o644); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "out.pdf")
	if err := (&pdfmerge.PDFCPU{}).Merge([]string{good, broken}, dest); err == nil {
		t.Fatal("merging a broken page: want error, got nil")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("a failed merge must not leave a half written document behind")
	}
}

func TestCommandExpandsPlaceholders(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `#!/bin/sh
out="$1"; shift
printf '%s\n' "$@" > "$out"
`)

	dest := filepath.Join(dir, "out.pdf")
	merger := &pdfmerge.Command{
		Name: script,
		Args: []string{"{{out}}", "--merge", "{{in}}"},
	}

	if err := merger.Merge([]string{"one.pdf", "two.pdf"}, dest); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	want := "--merge\none.pdf\ntwo.pdf\n"
	if string(got) != want {
		t.Fatalf("arguments = %q, want %q", got, want)
	}
}

func TestCommandAppendsPagesWithoutPlaceholder(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `#!/bin/sh
printf '%s\n' "$@" > `+filepath.Join(dir, "args.txt")+`
`)

	merger := &pdfmerge.Command{Name: script, Args: []string{"--flag"}}
	if err := merger.Merge([]string{"a.pdf", "b.pdf"}, filepath.Join(dir, "out.pdf")); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// Without {{out}} or {{in}} the pages are appended, matching tools such
	// as "pdfunite in... out".
	want := "--flag\na.pdf\nb.pdf\n"
	if string(got) != want {
		t.Fatalf("arguments = %q, want %q", got, want)
	}
}

func TestCommandFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `#!/bin/sh
echo "merge exploded" >&2
exit 3
`)

	dest := filepath.Join(dir, "out.pdf")
	err := (&pdfmerge.Command{Name: script}).Merge([]string{"a.pdf"}, dest)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "merge exploded") {
		t.Fatalf("error %q does not include the command output", err)
	}
}

func writeScript(t *testing.T, dir, body string) string {
	t.Helper()

	path := filepath.Join(dir, "merge.sh")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
