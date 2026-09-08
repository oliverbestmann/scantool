package keys_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/oliverbestmann/scantool/internal/keys"
)

// procDevices is a copy of /proc/bus/input/devices, including the USB keypad
// this daemon is meant to be driven with.
const procDevices = `I: Bus=0011 Vendor=0001 Product=0001 Version=ab83
N: Name="AT Translated Set 2 keyboard"
P: Phys=isa0060/serio0/input0
S: Sysfs=/devices/platform/i8042/serio0/input/input0
U: Uniq=
H: Handlers=sysrq kbd leds event0 
B: PROP=0
B: EV=120013

I: Bus=0003 Vendor=093a Product=2510 Version=0111
N: Name="PixArt USB Optical Mouse"
P: Phys=usb-0000:00:14.0-1/input0
S: Sysfs=/devices/pci0000:00/input/input5
U: Uniq=
H: Handlers=mouse0 event5 
B: PROP=0
B: EV=17

I: Bus=0003 Vendor=1a2c Product=0e24 Version=0110
N: Name="PCsensor MK321U Keyboard"
P: Phys=usb-0000:00:14.0-2.4.1.3/input0
S: Sysfs=/devices/pci0000:00/input/input19
U: Uniq=
H: Handlers=sysrq kbd leds event19 
B: PROP=0
B: EV=120013
`

func TestParseInputDevices(t *testing.T) {
	devices, err := keys.ParseInputDevices(strings.NewReader(procDevices))
	if err != nil {
		t.Fatalf("ParseInputDevices: %v", err)
	}
	if len(devices) != 3 {
		t.Fatalf("got %d devices, want 3", len(devices))
	}

	keypad := devices[2]
	if keypad.Name != "PCsensor MK321U Keyboard" {
		t.Errorf("name = %q", keypad.Name)
	}
	if keypad.Path != "/dev/input/event19" {
		t.Errorf("path = %q, want /dev/input/event19", keypad.Path)
	}
	if !keypad.IsKeyboard() {
		t.Error("the keypad should be recognised as a keyboard")
	}

	mouse := devices[1]
	if mouse.IsKeyboard() {
		t.Error("the mouse should not be recognised as a keyboard")
	}
	if mouse.Path != "/dev/input/event5" {
		t.Errorf("mouse path = %q", mouse.Path)
	}
}

func TestParseInputDevicesEmpty(t *testing.T) {
	devices, err := keys.ParseInputDevices(strings.NewReader(""))
	if err != nil {
		t.Fatalf("ParseInputDevices: %v", err)
	}
	if len(devices) != 0 {
		t.Fatalf("got %d devices, want none", len(devices))
	}
}

func TestParseInputDevicesWithoutTrailingBlankLine(t *testing.T) {
	const input = `I: Bus=0003
N: Name="Only Device"
H: Handlers=kbd event7`

	devices, err := keys.ParseInputDevices(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].Path != "/dev/input/event7" {
		t.Fatalf("devices = %+v, want the last block to be kept", devices)
	}
}

func TestRuneForCode(t *testing.T) {
	for code, want := range map[uint16]rune{30: 'a', 48: 'b', 46: 'c', 28: '\n', 79: '1'} {
		got, ok := keys.RuneForCode(code)
		if !ok || got != want {
			t.Errorf("RuneForCode(%d) = %q, %v, want %q, true", code, got, ok, want)
		}
	}

	if _, ok := keys.RuneForCode(190); ok {
		t.Error("RuneForCode(190) should be unmapped")
	}
}

func TestRuneForName(t *testing.T) {
	for name, want := range map[string]rune{"KEY_A": 'a', "KEY_B": 'b', "KEY_C": 'c', "KEY_KP0": '0'} {
		got, ok := keys.RuneForName(name)
		if !ok || got != want {
			t.Errorf("RuneForName(%q) = %q, %v, want %q, true", name, got, ok, want)
		}
	}

	if _, ok := keys.RuneForName("KEY_BRIGHTNESSUP"); ok {
		t.Error("KEY_BRIGHTNESSUP should be unmapped")
	}
}

func TestCodesForRune(t *testing.T) {
	if got := keys.CodesForRune('a'); len(got) != 1 || got[0] != 30 {
		t.Errorf("CodesForRune('a') = %v, want [30]", got)
	}
	// The number row and the keypad both produce a "1".
	if got := keys.CodesForRune('1'); len(got) != 2 {
		t.Errorf("CodesForRune('1') = %v, want the number row and the keypad", got)
	}
	if got := keys.CodesForRune('~'); len(got) != 0 {
		t.Errorf("CodesForRune('~') = %v, want none", got)
	}
}

func TestParseLibinputEvent(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantKey   rune
		wantState string
		wantOK    bool
	}{{
		name:      "pressed",
		line:      " event19  KEYBOARD_KEY     +0.000s\tKEY_A (30) pressed",
		wantKey:   'a',
		wantState: "pressed",
		wantOK:    true,
	}, {
		name:      "released",
		line:      " event19  KEYBOARD_KEY     +0.022s\tKEY_A (30) released",
		wantKey:   'a',
		wantState: "released",
		wantOK:    true,
	}, {
		name:      "keycode only",
		line:      " event19  KEYBOARD_KEY     +0.523s\tUNKNOWN (46) pressed",
		wantKey:   'c',
		wantState: "pressed",
		wantOK:    true,
	}, {
		// This is what libinput prints without --show-keycodes.
		name: "masked keycode",
		line: " event19  KEYBOARD_KEY     +0.000s\t*** (-1) pressed",
	}, {
		name: "unmapped key",
		line: " event19  KEYBOARD_KEY     +0.000s\tKEY_BRIGHTNESSUP (225) pressed",
	}, {
		name: "other event",
		line: "-event19  DEVICE_ADDED     PCsensor MK321U Keyboard  seat0 default group1  cap:k",
	}, {
		name: "empty",
		line: "",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, state, ok := keys.ParseLibinputEvent(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if key != tc.wantKey {
				t.Errorf("key = %q, want %q", key, tc.wantKey)
			}
			if state != tc.wantState {
				t.Errorf("state = %q, want %q", state, tc.wantState)
			}
		})
	}
}

func TestScanLibinputReportsPressesOnly(t *testing.T) {
	const output = `-event19  DEVICE_ADDED                 PCsensor MK321U Keyboard          seat0 default group1  cap:k
 event19  KEYBOARD_KEY                 +0.000s	KEY_A (30) pressed
 event19  KEYBOARD_KEY                 +0.022s	KEY_A (30) released
 event19  KEYBOARD_KEY                 +0.258s	KEY_B (48) pressed
 event19  KEYBOARD_KEY                 +0.279s	KEY_B (48) released
 event19  KEYBOARD_KEY                 +0.523s	KEY_C (46) pressed
 event19  KEYBOARD_KEY                 +0.544s	KEY_C (46) released
`

	var got []rune
	err := keys.ScanLibinput(t.Context(), strings.NewReader(output), func(key rune) bool {
		got = append(got, key)
		return true
	}, quiet())
	if err != nil {
		t.Fatalf("ScanLibinput: %v", err)
	}

	if string(got) != "abc" {
		t.Fatalf("keys = %q, want %q", string(got), "abc")
	}
}

func TestScanLibinputStopsWhenAsked(t *testing.T) {
	const output = ` event1  KEYBOARD_KEY  +0.000s	KEY_A (30) pressed
 event1  KEYBOARD_KEY  +0.100s	KEY_B (48) pressed
`

	var count int
	err := keys.ScanLibinput(t.Context(), strings.NewReader(output), func(rune) bool {
		count++
		return false
	}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("onKey called %d times, want 1", count)
	}
}

func TestLibinputName(t *testing.T) {
	if got := (&keys.Libinput{}).Name(); got != "libinput (all devices)" {
		t.Errorf("Name() = %q", got)
	}
	if got := (&keys.Libinput{Paths: []string{"/dev/input/event1"}}).Name(); !strings.Contains(got, "event1") {
		t.Errorf("Name() = %q", got)
	}
}

func TestLibinputReportsAMissingBinary(t *testing.T) {
	source := &keys.Libinput{Command: "definitely-not-installed-libinput", Logger: quiet()}

	err := source.Run(t.Context(), make(chan rune, 1))
	if err == nil {
		t.Fatal("want error for a missing libinput binary, got nil")
	}
}

func TestStdinDeliversKeys(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Raw mode is off: the pipe is not a terminal anyway.
	source := &keys.Stdin{In: r, Raw: false}
	if source.Name() != "stdin" {
		t.Fatalf("Name() = %q", source.Name())
	}

	out := make(chan rune, 8)
	done := make(chan error, 1)
	go func() { done <- source.Run(t.Context(), out) }()

	if _, err := w.WriteString("abc"); err != nil {
		t.Fatal(err)
	}

	for _, want := range []rune{'a', 'b', 'c'} {
		select {
		case got := <-out:
			if got != want {
				t.Fatalf("key = %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %q", want)
		}
	}

	// Closing the input ends the source.
	w.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return at end of input")
	}
}

func TestStdinQuitsOnCtrlC(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	source := &keys.Stdin{In: r, Raw: false}

	done := make(chan error, 1)
	go func() { done <- source.Run(t.Context(), make(chan rune, 4)) }()

	if _, err := w.Write([]byte{0x03}); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, keys.ErrQuit) {
			t.Fatalf("Run: %v, want ErrQuit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl-C did not stop the source")
	}
}

func TestStdinStopsOnCancel(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	defer r.Close()

	ctx, cancel := context.WithCancel(t.Context())
	source := &keys.Stdin{In: r, Raw: false}

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
		t.Fatal("Run did not return after cancellation")
	}
}

func TestStdinHandlesMultibyteInput(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	source := &keys.Stdin{In: r, Raw: false}
	out := make(chan rune, 8)
	go source.Run(t.Context(), out)

	if _, err := w.WriteString("ä\n"); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-out:
		if got != 'ä' {
			t.Fatalf("key = %q, want %q", got, 'ä')
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
