// Package pdfmerge combines the single page PDFs of a session into one file.
package pdfmerge

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// pdfcpu otherwise reads/writes a per-user config directory (and its fonts)
// on first use, which a headless daemon may not have access to (no HOME, a
// read-only filesystem, ...). Disable it; scantool doesn't need user fonts.
func init() {
	api.DisableConfigDir()
}

// Merger writes the pages, in order, as a single PDF to dest.
type Merger interface {
	Merge(pages []string, dest string) error
}

// Func adapts a plain function to the Merger interface.
type Func func(pages []string, dest string) error

func (f Func) Merge(pages []string, dest string) error { return f(pages, dest) }

// PDFCPU merges using the pdfcpu library, so the Pi needs no extra tooling.
type PDFCPU struct {
	// Conf is an optional pdfcpu configuration. nil uses the defaults.
	Conf *model.Configuration
}

// Merge implements Merger.
func (m *PDFCPU) Merge(pages []string, dest string) error {
	if len(pages) == 0 {
		return errors.New("pdfmerge: no pages to merge")
	}
	if err := prepare(dest); err != nil {
		return err
	}

	// A single page needs no merging; copying keeps the scanner output byte
	// for byte and avoids a pointless rewrite of the file.
	if len(pages) == 1 {
		return copyFile(pages[0], dest)
	}

	if err := api.MergeCreateFile(pages, dest, false, m.Conf); err != nil {
		os.Remove(dest)
		return fmt.Errorf("pdfmerge: merge %d pages: %w", len(pages), err)
	}
	return nil
}

// Command merges by shelling out to an external tool such as pdfunite. The
// placeholder {{out}} in Args is replaced by the destination, the page files
// are appended in place of {{in}} (or at the end if {{in}} is absent).
type Command struct {
	Name string
	Args []string
}

// Merge implements Merger.
func (c *Command) Merge(pages []string, dest string) error {
	if len(pages) == 0 {
		return errors.New("pdfmerge: no pages to merge")
	}
	if c.Name == "" {
		return errors.New("pdfmerge: no command configured")
	}
	if err := prepare(dest); err != nil {
		return err
	}

	var (
		args     []string
		expanded bool
	)
	for _, arg := range c.Args {
		switch arg {
		case "{{in}}":
			args = append(args, pages...)
			expanded = true
		case "{{out}}":
			args = append(args, dest)
		default:
			args = append(args, arg)
		}
	}
	if !expanded {
		args = append(args, pages...)
	}

	out, err := exec.Command(c.Name, args...).CombinedOutput()
	if err != nil {
		os.Remove(dest)
		return fmt.Errorf("pdfmerge: %s: %w: %s", c.Name, err, out)
	}
	return nil
}

func prepare(dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("pdfmerge: create output directory: %w", err)
	}
	return nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("pdfmerge: open page: %w", err)
	}
	defer in.Close()

	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("pdfmerge: create %s: %w", dest, err)
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dest)
		return fmt.Errorf("pdfmerge: copy page: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(dest)
		return fmt.Errorf("pdfmerge: close %s: %w", dest, err)
	}
	return nil
}
