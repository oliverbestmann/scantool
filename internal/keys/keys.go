// Package keys turns key presses from a terminal or a Linux input device into
// a stream of runes.
package keys

import (
	"context"
	"errors"
)

// ErrQuit is returned by a Source when the user asked the daemon to stop,
// e.g. by pressing Ctrl-C on a raw terminal.
var ErrQuit = errors.New("keys: quit requested")

// Source delivers key presses on out until ctx is cancelled.
type Source interface {
	// Run blocks, sending every key press to out. It returns nil when the
	// input ended, ErrQuit when the user asked to quit, or an error.
	Run(ctx context.Context, out chan<- rune) error
	// Name describes the source, for logging.
	Name() string
}

// send delivers a key unless ctx was cancelled first.
func send(ctx context.Context, out chan<- rune, key rune) bool {
	select {
	case out <- key:
		return true
	case <-ctx.Done():
		return false
	}
}
