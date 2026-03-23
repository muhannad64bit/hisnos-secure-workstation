#!/usr/bin/env bash
# boot/validate-kernel-cmdline.sh
#
# Validates that the kernel cmdline contains the HisnOS required flags.
# Used during post-install and can be run manually.
#
# Checks:
#   quiet         — suppresses verbose kernel messages
#   splash        — activates Plymouth
#   loglevel=3    — limits console noise to errors
#   rd.systemd.show_status=false — keeps dracut/systemd status off console
#
# Usage:
#   bash validate-kernel-cmdline.sh [--fix]
#
# With --fix: writes a GRUB default override and regenerates grub.cfg.

set -euo pipefail

FIX="${1:-}"
GRUB_DEFAULT="/etc/default/grub"
GRUB_CFG="/boot/grub2/grub.cfg"

REQUIRED_FLAGS=(
  "quiet"
  "splash"
  "loglevel=3"
  "rd.systemd.show_status=false"
)

CURRENT_CMDLINE=$(cat /proc/cmdline 2>/dev/null || echo "")

MISSING=()
for flag in "${REQUIRED_FLAGS[@]}"; do
  echo "${CURRENT_CMDLINE}" | grep -q "${flag}" || MISSING+=("${flag}")
done

if [[ "${#MISSING[@]}" -eq 0 ]]; then
  echo "[cmdline] All required flags present. Current cmdline OK."
  exit 0
fi

echo "[cmdline] Missing flags: ${MISSING[*]}"

if [[ "${FIX}" != "--fix" ]]; then
  echo "[cmdline] Run with --fix to add missing flags to GRUB config."
  echo "[cmdline] Affected boot entries may require: sudo grub2-mkconfig -o /boot/grub2/grub.cfg"
  exit 1
fi

[[ "$(id -u)" -eq 0 ]] || { echo "ERROR: --fix requires root" >&2; exit 1; }

# Build the additions string.
ADDITIONS="${MISSING[*]}"

# Patch /etc/default/grub if it exists (mutable Fedora / non-Kinoite).
if [[ -f "${GRUB_DEFAULT}" ]]; then
  # Append missing flags to GRUB_CMDLINE_LINUX if not already present.
  for flag in "${MISSING[@]}"; do
    if grep -q "^GRUB_CMDLINE_LINUX=" "${GRUB_DEFAULT}"; then
      if ! grep "GRUB_CMDLINE_LINUX" "${GRUB_DEFAULT}" | grep -q "${flag}"; then
        sed -i "s|^GRUB_CMDLINE_LINUX=\"\(.*\)\"|GRUB_CMDLINE_LINUX=\"\1 ${flag}\"|" "${GRUB_DEFAULT}"
        echo "[cmdline] Added '${flag}' to GRUB_CMDLINE_LINUX"
      fi
    fi
  done
  echo "[cmdline] Regenerating ${GRUB_CFG}..."
  grub2-mkconfig -o "${GRUB_CFG}"
  echo "[cmdline] Done. Reboot to apply."
else
  # Fedora Kinoite (BLS): patch via rpm-ostree kargs.
  echo "[cmdline] /etc/default/grub not found (immutable system). Patching via rpm-ostree kargs..."
  for flag in "${MISSING[@]}"; do
    rpm-ostree kargs --append="${flag}" 2>/dev/null || true
  done
  echo "[cmdline] rpm-ostree kargs updated. Reboot to apply."
fi
