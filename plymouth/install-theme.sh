#!/usr/bin/env bash
# plymouth/install-theme.sh — Install and activate the HisnOS Plymouth theme
#
# Safe on Fedora Kinoite (rpm-ostree): writes only to /usr/share/plymouth/themes/
# which is in the /usr filesystem overlay (managed via rpm-ostree kargs).
# For immutable Fedora Kinoite, the theme must be installed via an rpm-ostree overlay
# package OR injected into the initramfs at image build time.
#
# On a LIVE or development system: run directly as root.
# On rpm-ostree production: bundle theme files into an RPM and layer it.
#
# Usage: sudo bash install-theme.sh [--uninstall]

set -euo pipefail

THEME_NAME="hisnos"
THEME_SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")/hisnos" && pwd)"
THEME_DST="/usr/share/plymouth/themes/${THEME_NAME}"
ASSETS_SCRIPT="${THEME_SRC}/assets/generate-assets.sh"

[[ "$(id -u)" -eq 0 ]] || { echo "ERROR: must run as root" >&2; exit 1; }

uninstall() {
  if [[ -d "${THEME_DST}" ]]; then
    rm -rf "${THEME_DST}"
    echo "[hisnos-plymouth] theme removed from ${THEME_DST}"
  fi
  # Restore default theme
  plymouth-set-default-theme details 2>/dev/null || true
  echo "[hisnos-plymouth] default theme restored to 'details'"
}

if [[ "${1:-}" == "--uninstall" ]]; then
  uninstall
  exit 0
fi

# Generate assets if missing
if [[ ! -f "${THEME_SRC}/assets/logo.png" ]]; then
  echo "[hisnos-plymouth] generating theme assets..."
  bash "${ASSETS_SCRIPT}" --force
fi

# Install theme directory
mkdir -p "${THEME_DST}/assets"
cp "${THEME_SRC}/hisnos.plymouth" "${THEME_DST}/"
cp "${THEME_SRC}/hisnos.script"   "${THEME_DST}/"
cp "${THEME_SRC}/assets/"*.png    "${THEME_DST}/assets/"

echo "[hisnos-plymouth] theme installed to ${THEME_DST}"

# Set as default and rebuild initramfs
plymouth-set-default-theme "${THEME_NAME}" -R
echo "[hisnos-plymouth] initramfs rebuilt with theme '${THEME_NAME}'"

# Verify
ACTIVE="$(plymouth-set-default-theme)"
echo "[hisnos-plymouth] active theme: ${ACTIVE}"
[[ "${ACTIVE}" == "${THEME_NAME}" ]] || {
  echo "WARNING: active theme is '${ACTIVE}', expected '${THEME_NAME}'" >&2
}

echo ""
echo "Add to kernel cmdline (/etc/default/grub GRUB_CMDLINE_LINUX):"
echo "  quiet splash loglevel=3 rd.systemd.show_status=false"
echo "Then regenerate grub config:"
echo "  sudo grub2-mkconfig -o /boot/grub2/grub.cfg"
