package scan_test

import (
	"testing"

	"github.com/oliverbestmann/scantool/internal/scan"
)

func TestDevScannerImplementsTwoPhaseScanner(t *testing.T) {
	var _ scan.TwoPhaseScanner = (*scan.DevScanner)(nil)
}
