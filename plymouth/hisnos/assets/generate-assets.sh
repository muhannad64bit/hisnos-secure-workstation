#!/usr/bin/env bash
# plymouth/hisnos/assets/generate-assets.sh
#
# Generates Plymouth theme assets using ImageMagick (convert).
# Run once during package build or on first theme installation.
# Replace the generated files with production artwork for a real release.
#
# Dependencies: imagemagick
# Output files written to the same directory as this script.
#
# Usage: bash generate-assets.sh [--force]

set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

FORCE=false
[[ "${1:-}" == "--force" ]] && FORCE=true

need() {
  local f="$1"
  [[ "${FORCE}" == "true" ]] && return 0
  [[ ! -f "${f}" ]]
}

command -v convert &>/dev/null || {
  echo "ERROR: imagemagick 'convert' not found. Install: dnf install imagemagick" >&2
  exit 1
}

# ─────────────────────────────────────────────────────────────────
# background.png — 1×1 dark pixel (scaled to screen by Plymouth)
# #0a0a14 = very dark blue-black
# ─────────────────────────────────────────────────────────────────
if need "background.png"; then
  convert -size 1x1 xc:'#0a0a14' background.png
  echo "[+] background.png generated"
fi

# ─────────────────────────────────────────────────────────────────
# logo.png — 400×120 branded wordmark placeholder
# Uses DejaVu Bold for the text; cyan accent color
# Replace with actual SVG-rasterised logo for production
# ─────────────────────────────────────────────────────────────────
if need "logo.png"; then
  convert \
    -size 400x120 xc:'#0a0a14' \
    \
    -font DejaVu-Sans-Bold \
    -pointsize 52 \
    -fill '#00c8ff' \
    -gravity Center \
    -annotate +0-10 'HisnOS' \
    \
    -font DejaVu-Sans \
    -pointsize 14 \
    -fill '#556677' \
    -gravity Center \
    -annotate +0+38 'SECURE WORKSTATION' \
    \
    logo.png
  echo "[+] logo.png generated (placeholder — replace with production artwork)"
fi

# ─────────────────────────────────────────────────────────────────
# progress-track.png — 1×3 dim bar background (scaled by Plymouth)
# Very subtle — #1a2030 on the dark background
# ─────────────────────────────────────────────────────────────────
if need "progress-track.png"; then
  convert -size 1x3 xc:'#1a2030' progress-track.png
  echo "[+] progress-track.png generated"
fi

# ─────────────────────────────────────────────────────────────────
# progress.png — 1×3 bright fill (scaled to current progress width)
# Cyan gradient: #00c8ff → #0080cc
# ─────────────────────────────────────────────────────────────────
if need "progress.png"; then
  # Create a 380×3 gradient so Plymouth's Image.Scale stretches it nicely
  convert -size 380x3 gradient:'#00c8ff-#0072b8' progress.png
  echo "[+] progress.png generated"
fi

echo ""
echo "Assets ready. Install Plymouth theme:"
echo "  sudo cp -r $(dirname "${BASH_SOURCE[0]}")/../hisnos /usr/share/plymouth/themes/"
echo "  sudo plymouth-set-default-theme hisnos -R"
echo ""
echo "Kernel cmdline (add to /etc/default/grub GRUB_CMDLINE_LINUX):"
echo "  quiet splash loglevel=3 rd.systemd.show_status=false"
