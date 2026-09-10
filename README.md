# scantool

A small daemon for a headless Raspberry Pi with a scanner and a USB keypad
attached. It listens for key presses on the keypad and turns them into PDF
documents — no terminal, no SSH session, no desktop involved.

| Key | Action                                                                  |
|-----|-------------------------------------------------------------------------|
| `a` | Start a new document and scan its first page                            |
| `b` | Scan another page into the current document (press as often as you like) |
| `c` | Finish the document and store it as `<timestamp>.pdf`                    |

Two rules fall out of that:

* Pressing `a` while a document already holds pages stores that document
  first, exactly as if `c` had been pressed. Pressing `a` on a document
  without pages just retries the scan, so a failed scan never produces an
  empty file.
* Pressing `b` without an open document starts one.

A web page on `:8080` shows what the daemon is doing, the log of everything it
did, and the documents it produced.

## Building

```sh
make            # vet, test and build for this machine
make pi         # 64 bit Raspberry Pi OS  -> scantool-linux-arm64
make pi32       # 32 bit Raspberry Pi OS  -> scantool-linux-arm
```

Everything is pure Go — SQLite is `modernc.org/sqlite`, PDF merging is
`pdfcpu` — so the cross builds need no C toolchain. At runtime the daemon
itself shells out to `scanimage`, `magick` and `img2pdf` to scan a page (see
[Scanning a page](#scanning-a-page)), so those three need to be on `PATH`;
the Nix package wraps the binary with them already.

## Running

```sh
scantool --out /var/lib/scantool/scans
```

The flake's NixOS module (`nixosModules.default`) runs it as its own user via
systemd. The one thing that matters there is group membership: `input` to
read the keypad and `lp` to reach the scanner.

```sh
scantool --list-devices     # which keyboards the daemon can see
```

### Reading the keypad

The daemon reads Linux input devices directly (`--input evdev`, the default).
That works headlessly, needs no X11 and no libinput, and picks up keyboards
that are plugged in later. It needs read access to `/dev/input/event*`, which
the `input` group grants.

By default it listens on every auto-detected device that can actually send
`a`, `b` or `c`. That filter matters: the kernel binds its keyboard handler to
the power button, the lid switch, laptop hotkeys and a headset jack too, and
`--list-devices` shows which of them are worth listening to.

Auto-detection is still a blunt instrument — every keyboard on the machine
drives the scanner. Name the keypad instead:

```sh
scantool --device /dev/input/by-id/usb-PCsensor_MK321U-event-kbd
```

The paths under `/dev/input/by-id/` are stable across reboots and replugging,
unlike `/dev/input/eventN`.

#### Grabbing

`--grab` takes exclusive control of a device, so its key presses do not also
reach a console login prompt or the desktop:

| Value  | Behaviour                                                |
|--------|----------------------------------------------------------|
| `auto` | Grab only devices named with `--device` (default)        |
| `yes`  | Grab every device it listens to, including auto-detected |
| `no`   | Never grab                                               |

The default deliberately refuses to grab a keyboard you did not name: with
auto-detection that would take your own keyboard away from you.

A device is only ever grabbed while no key on it is held down. Grabbing with a
key still pressed swallows the release event — the press already reached the
desktop, the release does not — and since X11 and Wayland compositors repeat
keys in software, they then repeat that key forever. That is why starting the
daemon by pressing Enter used to leave Enter stuck.

Two other backends exist:

* `--input libinput` shells out to `libinput debug-events --show-keycodes`.
  Without `--show-keycodes` libinput masks the keys as `*** (-1)`, which is
  why this backend always passes it.
* `--input stdin` reads a terminal in raw mode. Handy for trying things out
  over SSH, useless for the real deployment.

### Scanning a page

By default scantool scans a page itself: `scanimage --format=pnm` into a
temp file, `magick convert -quality 95 -level 0%,90%` to a JPEG, then
`img2pdf` to wrap it into a single page PDF. It is configured entirely
through the environment:

| Variable          | Meaning                                  | Default              |
|-------------------|-------------------------------------------|---------------------|
| `SCAN_DEVICE`     | SANE device name, see `scanimage -L`      | scanimage's default |
| `SCAN_RESOLUTION` | dpi                                        | `300`                |
| `SCAN_MODE`       | `Color`, `Gray` or `Lineart`               | `Color`              |
| `SCAN_SOURCE`     | e.g. `Flatbed` or `ADF`                    | scanimage's default |

`--scan-command <cmd>` replaces all of that with an external script, called
once per page as `<cmd> <output.pdf>`, which must write a single page PDF to
that path. It also gets `SCANTOOL_DEST`, `SCANTOOL_SESSION` and
`SCANTOOL_PAGE` in the environment. Anything it prints is kept and shown in
the web UI when a scan fails.

A cancelled or timed out scan kills the whole process group, so a hanging
`scanimage` cannot keep the scanner busy.

### Storing documents

Pages are collected in `<out>/.scantool-work/session-<id>-*` and merged into
`<out>/<timestamp>.pdf` when the document is finished, where the timestamp is
the moment the document was started (`--name-layout` changes the format). If
that name is taken, `-2`, `-3` and so on are appended.

Merging uses the built-in pdfcpu. `--merge-command "pdfunite {{in}} {{out}}"`
switches to an external tool instead; `{{in}}` expands to the page files and
`{{out}}` to the destination.

If merging fails, the scanned pages are deliberately left in the work
directory and the path is written to the log, so nothing is ever lost.
Likewise, stopping the daemon with an open document stores that document
instead of dropping it.

## Web UI

`http://<pi>:8080/` shows the current state, the action log and the finished
documents; it polls `/api/state` every two seconds. The `a`, `b` and `c`
buttons queue the same actions as the keypad, which is convenient when you are
nowhere near the Pi. `--web-control=false` makes the page read only, `--http ""`
turns the server off entirely.

Sessions and the action log live in a SQLite database (`<out>/scantool.db`).
The log is trimmed to `--keep-actions` entries on startup.

## Tests

```sh
go test ./...
go test -race ./...
```

The tests need neither a scanner nor a keyboard:

* the scanner's `scanimage`/`magick`/`img2pdf` calls are stand-in shell
  scripts in tests, so its failure modes (bad exit code, no output, empty
  file, timeout, cancellation) are covered without SANE;
* the keypad is a FIFO that the evdev reader polls exactly like
  `/dev/input/eventN`, plus a pipe for the stdin backend;
* the PDFs are real PDFs, generated with pdfcpu and validated after merging;
* `main_test.go` drives the whole daemon end to end: key presses in, a
  four page PDF and a populated web UI out.

## Layout

| Path                | Contents                                             |
|---------------------|------------------------------------------------------|
| `main.go`           | Flags, wiring, the action loop and shutdown          |
| `internal/keys`     | Key sources: evdev, libinput, stdin                  |
| `internal/session`  | The state machine behind `a`, `b` and `c`            |
| `internal/scan`     | Scanning a page: built-in SANE scanner or `--scan-command` |
| `internal/pdfmerge` | Merging pages into a document                        |
| `internal/store`    | SQLite: sessions and the action log                  |
| `internal/web`      | Status page and JSON API                             |
