package scan_test

import (
	"context"
	"testing"

	"github.com/oliverbestmann/scantool/internal/scan"
)

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
