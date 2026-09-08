#!/bin/sh
# Scan a single page and write it to the file given as $1.
#
# scantool calls this script once per page:
#
#     scan-page.sh /path/to/page-001.pdf
#
# and additionally sets SCANTOOL_DEST, SCANTOOL_SESSION and SCANTOOL_PAGE.
# The script must write a single page PDF to $1 and exit non-zero on failure;
# anything it prints ends up in the daemon log and in the web UI.
#
# Configure it through the environment, e.g. in the systemd unit:
#
#     SCAN_DEVICE      SANE device name, see `scanimage -L` (default: first device)
#     SCAN_RESOLUTION  dpi (default: 300)
#     SCAN_MODE        Color, Gray or Lineart (default: Color)
#     SCAN_SOURCE      e.g. "Flatbed" or "ADF" (default: the scanner's default)

set -eu

dest="${1:?usage: scan-page.sh <output.pdf>}"

resolution="${SCAN_RESOLUTION:-300}"
mode="${SCAN_MODE:-Color}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

image="$tmp/page.png"

set -- --format=png --resolution "$resolution" --mode "$mode"
[ -n "${SCAN_DEVICE:-}" ] && set -- "$@" --device-name "$SCAN_DEVICE"
[ -n "${SCAN_SOURCE:-}" ] && set -- "$@" --source "$SCAN_SOURCE"

scanimage "$@" > "$image"

if [ ! -s "$image" ]; then
	echo "scanimage produced an empty image" >&2
	exit 1
fi

# img2pdf wraps the image without recompressing it, which keeps the scan sharp
# and the file small.
if ! command -v img2pdf > /dev/null 2>&1; then
	echo "img2pdf not found, install it" >&2
	exit 1
fi

img2pdf --output "$dest" "$image"
