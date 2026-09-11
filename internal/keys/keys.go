// Package keys turns key presses from a terminal or a Linux input device into
// a stream of runes.
package keys

import (
	"context"
	"errors"
	"time"
)

// ErrQuit is returned by a Source when the user asked the daemon to stop,
// e.g. by pressing Ctrl-C on a raw terminal.
var ErrQuit = errors.New("keys: quit requested")

// sendTimeout bounds how long send waits for the daemon to become ready to
// receive a key before giving up on it.
const sendTimeout = 5 * time.Second

// Source delivers key presses on out until ctx is cancelled.
type Source interface {
	// Run blocks, sending every key press to out. It returns nil when the
	// input ended, ErrQuit when the user asked to quit, or an error.
	Run(ctx context.Context, out chan<- rune) error
	// Name describes the source, for logging.
	Name() string
}

// send delivers a key to out, waiting up to sendTimeout for the daemon to be
// ready to receive it (e.g. while it is busy handling a previous key) before
// discarding it. It reports false only when ctx was cancelled first.
func send(ctx context.Context, out chan<- rune, key rune) bool {
	timer := time.NewTimer(sendTimeout)
	defer timer.Stop()

	select {
	case out <- key:
	case <-ctx.Done():
		return false
	case <-timer.C:
	}
	return true
}
