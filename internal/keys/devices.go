package keys

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// procInputDevices is the kernel's list of input devices.
const procInputDevices = "/proc/bus/input/devices"

// Device describes one Linux input device.
type Device struct {
	// Path is the character device, e.g. /dev/input/event3.
	Path string
	// Name is the human readable device name.
	Name string
	// Handlers are the kernel handlers bound to the device, e.g. kbd, mouse.
	Handlers []string
}

// IsKeyboard reports whether the kernel bound the keyboard handler to the
// device. That is what distinguishes a keyboard from a mouse or a lid switch.
func (d Device) IsKeyboard() bool {
	for _, h := range d.Handlers {
		if h == "kbd" {
			return true
		}
	}
	return false
}

// Keyboards lists the keyboard-like input devices of this machine.
func Keyboards() ([]Device, error) {
	f, err := os.Open(procInputDevices)
	if err != nil {
		return nil, fmt.Errorf("keys: open %s: %w", procInputDevices, err)
	}
	defer f.Close()

	devices, err := ParseInputDevices(f)
	if err != nil {
		return nil, err
	}

	var out []Device
	for _, d := range devices {
		if d.IsKeyboard() && d.Path != "" {
			out = append(out, d)
		}
	}
	return out, nil
}

// ParseInputDevices parses the format of /proc/bus/input/devices. Devices are
// separated by blank lines and described by prefixed lines, of which only
// N: (name) and H: (handlers) are interesting here:
//
//	I: Bus=0003 Vendor=04d9 Product=2013 Version=0110
//	N: Name="PCsensor MK321U Keyboard"
//	H: Handlers=sysrq kbd leds event19
func ParseInputDevices(r io.Reader) ([]Device, error) {
	var (
		devices []Device
		cur     Device
		started bool
	)

	flush := func() {
		if started {
			devices = append(devices, cur)
		}
		cur, started = Device{}, false
	}

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			flush()
			continue
		}

		prefix, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		started = true

		switch prefix {
		case "N":
			cur.Name = strings.Trim(strings.TrimPrefix(value, "Name="), `"`)
		case "H":
			cur.Handlers = strings.Fields(strings.TrimPrefix(value, "Handlers="))
			for _, h := range cur.Handlers {
				if strings.HasPrefix(h, "event") {
					cur.Path = "/dev/input/" + h
				}
			}
		}
	}
	flush()

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("keys: read input devices: %w", err)
	}
	return devices, nil
}
