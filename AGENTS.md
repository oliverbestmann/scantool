# AGENTS.md

## Manual UI testing: dev daemon + headless Chrome screenshots

`go test ./...` covers logic, but UI/layout changes to `internal/web/index.html`
need to actually be seen. There's no scanner or keypad in this environment, so
drive the real daemon through its dev flags and the web action API, then
screenshot it with headless Chromium.

### 1. Build and run a throwaway instance

```sh
go build -o /tmp/scantool .
rm -rf /tmp/scantool-data && mkdir -p /tmp/scantool-data
mkfifo /tmp/scantool-stdin 2>/dev/null

(exec 3<>/tmp/scantool-stdin; /tmp/scantool \
  --out /tmp/scantool-data --http :18080 \
  --dev-scanner --dev-lemmary \
  --input stdin --web-control=true \
  <&3 > /tmp/scantool.log 2>&1) &
sleep 1
```

Notes:you 

- `--dev-scanner` fakes the scanner (3s delay, blank page + thumbnail); no
  SANE/imagemagick/img2pdf needed.
- `--dev-lemmary` fakes the upload target (fails ~20% of the time, useful for
  exercising error states).
- `--input stdin` with a plain pipe closes immediately (EOF), which stops the
  daemon. Open it through a FIFO kept alive with `exec 3<>fifo` so the process
  stays up for the whole session.
- Use a scratch `--out` dir and non-default `--http` port so this never
  collides with a real deployment.

### 2. Drive it via the web action API, not real key presses

Actions are queued with `POST /api/action`, as form values (not JSON):

```sh
curl -s -X POST "http://localhost:18080/api/action?key=a"   # New
curl -s -X POST "http://localhost:18080/api/action?key=b"   # Add
curl -s -X POST "http://localhost:18080/api/action?key=c"   # Finish
curl -s -X POST "http://localhost:18080/api/action?action=discard"
curl -s -X POST "http://localhost:18080/api/action?action=finish-no-upload"
```

Each dev-scanner scan takes ~3s; `sleep 4` between actions before checking
state or screenshotting. `curl -s http://localhost:18080/api/state` dumps the
full JSON state/session/action-log snapshot, handy for confirming a change
before bothering with a screenshot at all.

### 3. Screenshot with headless Chromium

```sh
/home/oliver/.local/bin/chromium --headless --disable-gpu --no-sandbox \
  --screenshot=/tmp/page.png --window-size=420,420 \
  http://localhost:18080/
```

Then view it with the Read tool (it renders images directly). Vary
`--window-size` to check narrow-viewport behavior (e.g. horizontal scroll
strips, wrapping). The `SharedImageManager::ProduceMemory` GPU error on
stderr is harmless in this sandboxed/headless environment — ignore it.

### 4. Clean up

```sh
pkill -f "/tmp/scantool --out"
rm -f /tmp/scantool-stdin
rm -rf /tmp/scantool-data /tmp/scantool.log /tmp/scantool /tmp/*.png
```

`pkill` against a backgrounded shell function can print a benign "exit code
144" — that's just the killed process's own exit status, not a tool failure.

### When to actually do this

Skip it for pure Go logic changes covered by `go test ./...`. Do it whenever
a change touches `internal/web/index.html` layout/CSS/template logic,
morphdom diffing behavior, or anything else where "does it look right" can't
be answered by reading the diff.
