#!/usr/bin/env bash
# recovery/hisnos-recovery-setup.sh
#
# Installs the HisnOS recovery GRUB entry and dracut module, then
# regenerates the GRUB configuration and initramfs.
#
# Usage:
#   sudo bash hisnos-recovery-setup.sh [--uninstall]
#
# Idempotent: safe to re-run.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

[[ "$(id -u)" -eq 0 ]] || { echo "ERROR: must run as root" >&2; exit 1; }

GRUB_ENTRY_SRC="${SCRIPT_DIR}/grub.d/41_hisnos-recovery"
GRUB_ENTRY_DST="/etc/grub.d/41_hisnos-recovery"

DRACUT_SRC="${SCRIPT_DIR}/dracut/95hisnos-recovery"
DRACUT_DST="/usr/lib/dracut/modules.d/95hisnos-recovery"

uninstall() {
  echo "[hisnos-recovery] Removing GRUB entry…"
  rm -f "${GRUB_ENTRY_DST}"

  echo "[hisnos-recovery] Removing dracut module…"
  rm -rf "${DRACUT_DST}"

  echo "[hisnos-recovery] Regenerating GRUB config…"
  grub2-mkconfig -o /boot/grub2/grub.cfg

  KVER=$(uname -r)
  echo "[hisnos-recovery] Rebuilding initramfs for ${KVER}…"
  dracut --force "/boot/initramfs-${KVER}.img" "${KVER}"

  echo "[hisnos-recovery] Uninstall complete."
}

if [[ "${1:-}" == "--uninstall" ]]; then
  uninstall
  exit 0
fi

# ── Install GRUB entry ────────────────────────────────────────────────────────
echo "[hisnos-recovery] Installing GRUB entry…"
install -m 0755 "${GRUB_ENTRY_SRC}" "${GRUB_ENTRY_DST}"

# ── Install dracut module ──────────────────────────────────────────────────────
echo "[hisnos-recovery] Installing dracut module…"
install -d "${DRACUT_DST}"
install -m 0755 "${DRACUT_SRC}/module-setup.sh"    "${DRACUT_DST}/module-setup.sh"
install -m 0755 "${DRACUT_SRC}/hisnos-recovery.sh" "${DRACUT_DST}/hisnos-recovery.sh"

# ── Regenerate GRUB config ────────────────────────────────────────────────────
echo "[hisnos-recovery] Regenerating GRUB config…"
grub2-mkconfig -o /boot/grub2/grub.cfg

# ── Rebuild initramfs ─────────────────────────────────────────────────────────
KVER=$(uname -r)
echo "[hisnos-recovery] Rebuilding initramfs for ${KVER}…"
dracut --force --add "hisnos-recovery" "/boot/initramfs-${KVER}.img" "${KVER}"

echo ""
echo "[hisnos-recovery] Setup complete."
echo "  Recovery entry will appear in the GRUB menu on next boot."
echo "  Test with: grep -A5 'HisnOS Recovery' /boot/grub2/grub.cfg"
