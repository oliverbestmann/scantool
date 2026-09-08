package keys

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	"golang.org/x/term"
)

// Stdin reads key presses from standard input. When stdin is a terminal it is
// switched to raw mode, so single key presses arrive without pressing enter.
type Stdin struct {
	// In is the input to read from. Defaults to os.Stdin.
	In *os.File
	// Raw disables raw mode when set to false. Defaults to true.
	Raw bool
}

// NewStdin returns a Stdin source reading from os.Stdin in raw mode.
func NewStdin() *Stdin {
	return &Stdin{In: os.Stdin, Raw: true}
}

// Name implements Source.
func (s *Stdin) Name() string { return "stdin" }

// Run implements Source.
func (s *Stdin) Run(ctx context.Context, out chan<- rune) error {
	in := s.In
	if in == nil {
		in = os.Stdin
	}

	if s.Raw && term.IsTerminal(int(in.Fd())) {
		restore, err := term.MakeRaw(int(in.Fd()))
		if err != nil {
			return fmt.Errorf("keys: raw mode: %w", err)
		}
		defer term.Restore(int(in.Fd()), restore)
	}

	// Reads on stdin cannot be cancelled, so they happen in their own
	// goroutine and Run just stops listening when the context ends.
	type result struct {
		key rune
		err error
	}
	results := make(chan result)

	go func() {
		defer close(results)

		reader := newRuneReader(in)
		for {
			key, err := reader.next()
			select {
			case results <- result{key: key, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil

		case res, ok := <-results:
			if !ok {
				return nil
			}
			if res.err != nil {
				if errors.Is(res.err, io.EOF) {
					return nil
				}
				return res.err
			}
			// In raw mode the terminal no longer generates SIGINT, so
			// Ctrl-C and Ctrl-D have to be handled here.
			switch res.key {
			case 0x03, 0x04:
				return ErrQuit
			}
			if !send(ctx, out, res.key) {
				return nil
			}
		}
	}
}

// runeReader decodes UTF-8 runes from a byte stream one at a time, without
// buffering ahead, so a key press is never held back waiting for more input.
type runeReader struct {
	r   io.Reader
	buf []byte
}

func newRuneReader(r io.Reader) *runeReader {
	return &runeReader{r: r}
}

func (rr *runeReader) next() (rune, error) {
	for {
		if r, size := utf8.DecodeRune(rr.buf); r != utf8.RuneError || size > 1 {
			rr.buf = rr.buf[size:]
			return r, nil
		}
		if len(rr.buf) >= utf8.UTFMax {
			// Not valid UTF-8, drop a byte and carry on.
			rr.buf = rr.buf[1:]
			continue
		}

		var chunk [64]byte
		n, err := rr.r.Read(chunk[:])
		rr.buf = append(rr.buf, chunk[:n]...)
		if err != nil {
			if len(rr.buf) > 0 && errors.Is(err, io.EOF) {
				// Flush whatever is left as raw bytes.
				b := rr.buf[0]
				rr.buf = rr.buf[1:]
				return rune(b), nil
			}
			return 0, err
		}
	}
}
