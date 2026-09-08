package keys

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// encodeEvent builds a struct input_event the way the kernel would.
func encodeEvent(typ, code uint16, value int32) []byte {
	buf := make([]byte, eventSize)
	binary.NativeEndian.PutUint16(buf[eventSize-8:], typ)
	binary.NativeEndian.PutUint16(buf[eventSize-6:], code)
	binary.NativeEndian.PutUint32(buf[eventSize-4:], uint32(value))
	return buf
}

func TestEventSizeMatchesTheKernelStruct(t *testing.T) {
	// struct input_event is two __kernel_ulong_t timestamps plus type, code
	// and value: 24 bytes on 64 bit, 16 bytes on a 32 bit Raspberry Pi.
	want := 24
	if wordBytes == 4 {
		want = 16
	}
	if eventSize != want {
		t.Fatalf("eventSize = %d, want %d for %d bit words", eventSize, want, wordBytes*8)
	}
}

func TestDecodeEvent(t *testing.T) {
	typ, code, value := decodeEvent(encodeEvent(evKey, 30, valueDown))

	if typ != evKey {
		t.Errorf("type = %d, want %d", typ, evKey)
	}
	if code != 30 {
		t.Errorf("code = %d, want 30", code)
	}
	if value != valueDown {
		t.Errorf("value = %d, want %d", value, valueDown)
	}
}

func TestDecodeEventNegativeValue(t *testing.T) {
	if _, _, value := decodeEvent(encodeEvent(0x02, 0, -3)); value != -3 {
		t.Fatalf("value = %d, want -3", value)
	}
}

// TestEvdevReadsKeyPresses drives the real read loop through a FIFO, which
// behaves like /dev/input/eventN as far as poll and read are concerned. That
// way the loop is covered without a keyboard or root privileges.
func TestEvdevReadsKeyPresses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "event0")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create fifo: %v", err)
	}

	source := &Evdev{
		Paths:          []string{path},
		RescanInterval: 20 * time.Millisecond,
		Logger:         quietLogger(),
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	out := make(chan rune, 16)
	done := make(chan error, 1)
	go func() { done <- source.Run(ctx, out) }()

	// Opening the write end blocks until the daemon has opened the device.
	w, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open fifo for writing: %v", err)
	}

	var events []byte
	events = append(events, encodeEvent(evKey, 30, valueDown)...)   // a
	events = append(events, encodeEvent(evKey, 30, valueUp)...)     // ignored
	events = append(events, encodeEvent(evKey, 48, valueDown)...)   // b
	events = append(events, encodeEvent(evKey, 48, valueRepeat)...) // ignored
	events = append(events, encodeEvent(0x02, 0, 1)...)             // not a key
	events = append(events, encodeEvent(evKey, 190, valueDown)...)  // unmapped
	events = append(events, encodeEvent(evKey, 46, valueDown)...)   // c

	if _, err := w.Write(events); err != nil {
		t.Fatalf("write events: %v", err)
	}

	for _, want := range []rune{'a', 'b', 'c'} {
		select {
		case got := <-out:
			if got != want {
				t.Fatalf("key = %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for key %q", want)
		}
	}

	w.Close()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

func TestEvdevStopsOnCancelWithoutDevices(t *testing.T) {
	source := &Evdev{
		Paths:          []string{filepath.Join(t.TempDir(), "does-not-exist")},
		RescanInterval: 20 * time.Millisecond,
		Logger:         quietLogger(),
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- source.Run(ctx, make(chan rune)) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

func TestEvdevName(t *testing.T) {
	if got := (&Evdev{}).Name(); got != "evdev (autodetect)" {
		t.Errorf("Name() = %q", got)
	}
	if got := (&Evdev{Paths: []string{"/dev/input/event3"}}).Name(); got != "evdev /dev/input/event3" {
		t.Errorf("Name() = %q", got)
	}
}

// TestEvdevKeepsReadingWhenGrabbingFails covers the grab path over a FIFO,
// which is not an input device: reading the key state fails, the daemon logs
// it and carries on reading keys instead of giving up.
func TestEvdevKeepsReadingWhenGrabbingFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "event0")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create fifo: %v", err)
	}

	source := &Evdev{
		Paths:          []string{path},
		Grab:           true,
		RescanInterval: 20 * time.Millisecond,
		Logger:         quietLogger(),
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	out := make(chan rune, 4)
	done := make(chan error, 1)
	go func() { done <- source.Run(ctx, out) }()

	w, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open fifo for writing: %v", err)
	}
	defer w.Close()

	if _, err := w.Write(encodeEvent(evKey, 30, valueDown)); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-out:
		if got != 'a' {
			t.Fatalf("key = %q, want %q", got, 'a')
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no key arrived after the grab failed")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestAnyKeyDownNeedsAnInputDevice pins down that the key state check reports
// an error for anything that is not an input device, rather than claiming no
// key is held and grabbing anyway.
func TestAnyKeyDownNeedsAnInputDevice(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "not-a-device")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if _, err := anyKeyDown(int(f.Fd())); err == nil {
		t.Fatal("want an error for a regular file, got nil")
	}
}

// TestGrabIdleDevicesWaitsForRelease uses a real input device if one is
// readable, and checks the daemon does not grab while a key is held. Grabbing
// a device with a key still down swallows the release, which leaves the
// desktop repeating that key forever.
func TestGrabIdleDevicesWaitsForRelease(t *testing.T) {
	devices, err := Keyboards()
	if err != nil || len(devices) == 0 {
		t.Skip("no input devices to test against")
	}

	fd := -1
	for _, d := range devices {
		if opened, err := unix.Open(d.Path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0); err == nil {
			fd = opened
			break
		}
	}
	if fd < 0 {
		t.Skip("no readable input device, needs membership in the input group")
	}
	defer unix.Close(fd)

	// The state of a real keyboard is readable, which is what the grab
	// decision depends on.
	if _, err := anyKeyDown(fd); err != nil {
		t.Fatalf("anyKeyDown on a real keyboard: %v", err)
	}
}

func TestEviocgkeyRequestNumber(t *testing.T) {
	// EVIOCGKEY(96) is _IOR('E', 0x18, 96), which the kernel headers spell
	// out as 0x80604518.
	if eviocgkey != 0x80604518 {
		t.Fatalf("eviocgkey = %#x, want 0x80604518", eviocgkey)
	}
	if keyStateBytes != 96 {
		t.Fatalf("keyStateBytes = %d, want 96", keyStateBytes)
	}
}

func TestEviocgbitKeyRequestNumber(t *testing.T) {
	// EVIOCGBIT(EV_KEY, 96) is _IOR('E', 0x21, 96) = 0x80604521.
	if eviocgbitKey != 0x80604521 {
		t.Fatalf("eviocgbitKey = %#x, want 0x80604521", eviocgbitKey)
	}
}

func TestCanProduceNeedsAnInputDevice(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "not-a-device")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if _, err := canProduce(int(f.Fd()), []rune{'a'}); err == nil {
		t.Fatal("want an error for a regular file, got nil")
	}
}

// TestEvdevDoesNotFilterNamedDevices makes sure the capability filter only
// applies to auto-detection: a device given with --device is opened even
// though its capabilities cannot be read.
func TestEvdevDoesNotFilterNamedDevices(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "event0")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create fifo: %v", err)
	}

	source := &Evdev{
		Paths:          []string{path},
		RequireKeys:    []rune{'a', 'b', 'c'},
		RescanInterval: 20 * time.Millisecond,
		Logger:         quietLogger(),
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	out := make(chan rune, 4)
	go source.Run(ctx, out)

	w, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open fifo for writing: %v", err)
	}
	defer w.Close()

	if _, err := w.Write(encodeEvent(evKey, 48, valueDown)); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-out:
		if got != 'b' {
			t.Fatalf("key = %q, want %q", got, 'b')
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a device named explicitly should not be filtered out")
	}
}
