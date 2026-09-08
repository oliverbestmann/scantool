package keys

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Size of struct input_event. The two leading timeval fields are
// __kernel_ulong_t, which matches Go's int size on Linux: 24 bytes on 64 bit,
// 16 bytes on a 32 bit Raspberry Pi OS.
const wordBytes = strconv.IntSize / 8
const eventSize = 2*wordBytes + 8

// eviocgrab is EVIOCGRAB, _IOW('E', 0x90, int): it gives this process
// exclusive access to a device, so key presses do not also reach the console.
const eviocgrab = 0x40044590

// keyStateBytes is KEY_MAX/8+1, the size of the key state bitmask.
const keyStateBytes = (0x2ff / 8) + 1

// eviocgkey is EVIOCGKEY(keyStateBytes), _IOR('E', 0x18, len): it reads which
// keys of a device are held down right now.
const eviocgkey = 0x80000000 | (keyStateBytes << 16) | (0x45 << 8) | 0x18

// eviocgbitKey is EVIOCGBIT(EV_KEY, keyStateBytes), _IOR('E', 0x20+EV_KEY,
// len): it reads which keys a device is able to report at all.
const eviocgbitKey = 0x80000000 | (keyStateBytes << 16) | (0x45 << 8) | (0x20 + evKey)

// Evdev reads key presses straight from Linux input devices. This is the
// backend for headless operation: it needs no terminal, no X11 and no
// libinput, only read access to /dev/input/event*.
type Evdev struct {
	// Paths are the devices to read. Symlinks such as
	// /dev/input/by-id/usb-...-event-kbd are fine. Empty means every
	// keyboard reported by /proc/bus/input/devices.
	Paths []string
	// Grab takes exclusive ownership of the devices, so the key presses do
	// not leak into a login shell on the console. A device is only grabbed
	// once no key on it is held down, see grabIdleDevices.
	Grab bool
	// RequireKeys filters auto-detected devices down to those that can
	// actually produce these keys. The kernel calls a power button and a
	// laptop's hotkeys keyboards too, and listening to those is at best
	// pointless. Devices named in Paths are never filtered.
	RequireKeys []rune
	// RescanInterval controls how often new devices are picked up, so a
	// keyboard can be plugged in after the daemon started. Defaults to 3s.
	RescanInterval time.Duration
	// Logger receives diagnostics. Defaults to slog.Default().
	Logger *slog.Logger
}

// Name implements Source.
func (e *Evdev) Name() string {
	if len(e.Paths) == 0 {
		return "evdev (autodetect)"
	}
	return "evdev " + strings.Join(e.Paths, ",")
}

type evdevDevice struct {
	path string
	fd   int
	// grabbed is set once the device is exclusively ours, or once grabbing
	// it has been given up on.
	grabbed bool
}

// Run implements Source. It keeps running while devices come and go, and only
// returns when ctx is cancelled or the devices cannot be watched at all.
func (e *Evdev) Run(ctx context.Context, out chan<- rune) error {
	log := e.Logger
	if log == nil {
		log = slog.Default()
	}

	rescan := e.RescanInterval
	if rescan <= 0 {
		rescan = 3 * time.Second
	}

	// A pipe lets a cancelled context interrupt the poll below.
	var wake [2]int
	if err := unix.Pipe2(wake[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		return fmt.Errorf("keys: create wakeup pipe: %w", err)
	}
	defer unix.Close(wake[0])
	defer unix.Close(wake[1])

	stop := context.AfterFunc(ctx, func() {
		unix.Write(wake[1], []byte{0})
	})
	defer stop()

	open := map[string]*evdevDevice{}
	defer func() {
		for _, dev := range open {
			unix.Close(dev.fd)
		}
	}()

	warnedEmpty := false
	retryAfter := map[string]time.Time{}
	skipped := map[string]bool{}
	buf := make([]byte, eventSize*64)

	for ctx.Err() == nil {
		e.refresh(open, retryAfter, skipped, log)

		// Devices are grabbed here rather than on open, so that the key that
		// started the daemon has a chance to be released first.
		e.grabIdleDevices(open, log)

		if len(open) == 0 && !warnedEmpty {
			log.Warn("no input devices available, waiting for one to appear",
				"paths", strings.Join(e.Paths, ","))
			warnedEmpty = true
		} else if len(open) > 0 {
			warnedEmpty = false
		}

		fds := make([]unix.PollFd, 0, len(open)+1)
		fds = append(fds, unix.PollFd{Fd: int32(wake[0]), Events: unix.POLLIN})

		devs := make([]*evdevDevice, 0, len(open))
		for _, dev := range sortedDevices(open) {
			fds = append(fds, unix.PollFd{Fd: int32(dev.fd), Events: unix.POLLIN})
			devs = append(devs, dev)
		}

		n, err := unix.Poll(fds, int(rescan/time.Millisecond))
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("keys: poll input devices: %w", err)
		}
		if n == 0 {
			continue // timed out, loop around and rescan
		}

		if fds[0].Revents != 0 {
			return nil // context cancelled
		}

		for i, dev := range devs {
			revents := fds[i+1].Revents
			if revents == 0 {
				continue
			}
			if revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
				log.Info("input device disconnected", "device", dev.path)
				closeDevice(open, retryAfter, dev, rescan)
				continue
			}

			keep, err := e.readDevice(ctx, dev, buf, out, log)
			if err != nil {
				log.Warn("input device read failed", "device", dev.path, "error", err)
			}
			if !keep {
				closeDevice(open, retryAfter, dev, rescan)
			}
			if ctx.Err() != nil {
				return nil
			}
		}
	}

	return nil
}

// readDevice drains the pending events of one device. It reports whether the
// device should stay open.
func (e *Evdev) readDevice(ctx context.Context, dev *evdevDevice, buf []byte, out chan<- rune, log *slog.Logger) (bool, error) {
	n, err := unix.Read(dev.fd, buf)
	switch {
	case errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR):
		return true, nil
	case err != nil:
		// ENODEV means the keyboard was unplugged; it may come back.
		return false, err
	case n == 0:
		return false, nil
	}

	for off := 0; off+eventSize <= n; off += eventSize {
		typ, code, value := decodeEvent(buf[off : off+eventSize])
		if typ != evKey || (value != valueDown && value != valueRepeat) {
			continue
		}
		// Auto repeat would scan a page per repeat; only the initial press
		// counts.
		if value == valueRepeat {
			continue
		}

		key, ok := RuneForCode(code)
		if !ok {
			log.Debug("ignoring unmapped key", "device", dev.path, "code", code)
			continue
		}
		if !send(ctx, out, key) {
			return true, nil
		}
	}

	return true, nil
}

// decodeEvent extracts type, code and value from a struct input_event. The
// timestamp is not used, so the layout only matters up to its size.
func decodeEvent(b []byte) (typ, code uint16, value int32) {
	typ = binary.NativeEndian.Uint16(b[eventSize-8 : eventSize-6])
	code = binary.NativeEndian.Uint16(b[eventSize-6 : eventSize-4])
	value = int32(binary.NativeEndian.Uint32(b[eventSize-4 : eventSize]))
	return typ, code, value
}

// refresh opens devices that are configured but not open yet. Devices that
// just went away are left alone until retryAfter has passed, so a device that
// keeps reporting a hangup cannot spin this loop.
func (e *Evdev) refresh(open map[string]*evdevDevice, retryAfter map[string]time.Time, skipped map[string]bool, log *slog.Logger) {
	paths, err := e.devicePaths()
	if err != nil {
		log.Warn("could not list input devices", "error", err)
		return
	}

	for _, path := range paths {
		if _, ok := open[path]; ok {
			continue
		}
		if skipped[path] {
			continue
		}
		if until, ok := retryAfter[path]; ok && time.Now().Before(until) {
			continue
		}
		delete(retryAfter, path)

		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			// Missing permissions are the usual cause, and repeating the
			// warning every rescan would flood the log.
			log.Debug("could not open input device", "device", path, "error", err)
			continue
		}

		// Auto-detected devices have to prove they can send the keys the
		// daemon acts on. An explicitly named device is taken as given.
		if len(e.Paths) == 0 && len(e.RequireKeys) > 0 {
			usable, err := canProduce(fd, e.RequireKeys)
			if err != nil {
				log.Debug("could not read key capabilities", "device", path, "error", err)
			}
			if err == nil && !usable {
				log.Debug("ignoring input device, it cannot send the keys we listen for",
					"device", path, "name", deviceName(path))
				unix.Close(fd)
				skipped[path] = true
				continue
			}
		}

		open[path] = &evdevDevice{path: path, fd: fd}
		log.Info("listening on input device", "device", path, "name", deviceName(path))
	}
}

// grabIdleDevices takes exclusive control of the devices that are not grabbed
// yet, but only while no key on them is held down.
//
// Grabbing with a key still pressed is what makes a key appear stuck: the
// press was already delivered to the console or the desktop, the release goes
// only to this process, and X11 and Wayland compositors, which repeat keys in
// software, then repeat that key forever. Waiting for an idle device costs
// nothing, because the release event that ends the wait is exactly what wakes
// the poll loop up.
func (e *Evdev) grabIdleDevices(open map[string]*evdevDevice, log *slog.Logger) {
	if !e.Grab {
		return
	}

	for _, dev := range sortedDevices(open) {
		if dev.grabbed {
			continue
		}

		down, err := anyKeyDown(dev.fd)
		if err != nil {
			// Not an evdev device, or an old kernel: better to keep reading
			// it than to risk grabbing it blindly.
			log.Warn("could not read key state, not grabbing device",
				"device", dev.path, "error", err)
			dev.grabbed = true
			continue
		}
		if down {
			log.Debug("waiting for all keys to be released before grabbing", "device", dev.path)
			continue
		}

		dev.grabbed = true
		if err := unix.IoctlSetInt(dev.fd, eviocgrab, 1); err != nil {
			log.Warn("could not grab input device", "device", dev.path, "error", err)
			continue
		}
		log.Info("grabbed input device exclusively", "device", dev.path)
	}
}

// anyKeyDown reports whether any key of the device is currently held down.
func anyKeyDown(fd int) (bool, error) {
	var state [keyStateBytes]byte

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(eviocgkey),
		uintptr(unsafe.Pointer(&state[0])))
	if errno != 0 {
		return false, errno
	}

	for _, b := range state {
		if b != 0 {
			return true, nil
		}
	}
	return false, nil
}

// canProduce reports whether an open device is able to send every one of the
// given keys.
func canProduce(fd int, required []rune) (bool, error) {
	var bits [keyStateBytes]byte

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(eviocgbitKey),
		uintptr(unsafe.Pointer(&bits[0])))
	if errno != 0 {
		return false, errno
	}

	for _, key := range required {
		found := false
		for _, code := range CodesForRune(key) {
			if int(code)/8 < len(bits) && bits[code/8]&(1<<(code%8)) != 0 {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}

// CanProduce reports whether the device at path can send every one of the
// given keys. It is used to explain, in --list-devices, which devices are
// worth listening to.
func CanProduce(path string, required []rune) (bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)

	return canProduce(fd, required)
}

// devicePaths resolves the configured devices, or auto-detects keyboards.
func (e *Evdev) devicePaths() ([]string, error) {
	if len(e.Paths) > 0 {
		var paths []string
		for _, p := range e.Paths {
			// Resolve /dev/input/by-id symlinks so a device is not opened
			// twice under two names.
			if resolved, err := filepath.EvalSymlinks(p); err == nil {
				p = resolved
			}
			paths = append(paths, p)
		}
		return paths, nil
	}

	devices, err := Keyboards()
	if err != nil {
		return nil, err
	}

	var paths []string
	for _, d := range devices {
		paths = append(paths, d.Path)
	}
	return paths, nil
}

// closeDevice drops a device and holds off reopening it for one rescan
// interval.
func closeDevice(open map[string]*evdevDevice, retryAfter map[string]time.Time, dev *evdevDevice, rescan time.Duration) {
	unix.Close(dev.fd)
	delete(open, dev.path)
	retryAfter[dev.path] = time.Now().Add(rescan)
}

func sortedDevices(open map[string]*evdevDevice) []*evdevDevice {
	devs := make([]*evdevDevice, 0, len(open))
	for _, dev := range open {
		devs = append(devs, dev)
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].path < devs[j].path })
	return devs
}

func deviceName(path string) string {
	// /sys/class/input/eventN/device/name holds the human readable name.
	name, err := os.ReadFile(filepath.Join("/sys/class/input", filepath.Base(path), "device", "name"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(name))
}
