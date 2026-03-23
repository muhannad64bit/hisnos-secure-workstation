#!/usr/bin/env bash
# boot/plymouth-fallback.sh
#
# Validates the Plymouth theme and falls back to 'text' if the hisnos theme
# is broken, missing assets, or plymouth is not installed.
#
# Run during post-install (bootstrap-installer.sh step 13) and after
# plymouth-set-default-theme to ensure the system always boots visually.
#
# Exit codes:
#   0  — hisnos theme active and valid
#   1  — fallback applied (text or details)

set -euo pipefail

THEME="hisnos"
THEME_DIR="/usr/share/plymouth/themes/${THEME}"
REQUIRED_ASSETS=(
  "${THEME_DIR}/hisnos.plymouth"
  "${THEME_DIR}/hisnos.script"
  "${THEME_DIR}/assets/background.png"
  "${THEME_DIR}/assets/logo.png"
  "${THEME_DIR}/assets/progress-track.png"
  "${THEME_DIR}/assets/progress.png"
)
FALLBACK_THEME="text"

log()  { echo "[hisnos-plymouth] $*"; }
warn() { echo "[hisnos-plymouth WARN] $*" >&2; }

# Must run as root.
[[ "$(id -u)" -eq 0 ]] || { echo "ERROR: must run as root" >&2; exit 1; }

# ── Check plymouth is installed ───────────────────────────────────────────────
if ! command -v plymouth-set-default-theme &>/dev/null; then
  warn "plymouth not installed — boot splash unavailable"
  exit 1
fi

# ── Validate theme directory and required files ───────────────────────────────
MISSING=()
for f in "${REQUIRED_ASSETS[@]}"; do
  [[ -f "${f}" ]] || MISSING+=("${f}")
done

if [[ "${#MISSING[@]}" -gt 0 ]]; then
  warn "Theme '${THEME}' is missing assets:"
  for m in "${MISSING[@]}"; do
    warn "  - ${m}"
  done
  warn "Attempting to generate assets..."

  ASSETS_SCRIPT="${THEME_DIR}/assets/generate-assets.sh"
  if [[ -f "${ASSETS_SCRIPT}" ]]; then
    bash "${ASSETS_SCRIPT}" --force && {
      log "Assets generated successfully"
      MISSING=()
    } || true
  fi

  # Re-check after generation attempt.
  STILL_MISSING=()
  for f in "${REQUIRED_ASSETS[@]}"; do
    [[ -f "${f}" ]] || STILL_MISSING+=("${f}")
  done

  if [[ "${#STILL_MISSING[@]}" -gt 0 ]]; then
    warn "Assets still missing after generation attempt. Falling back to '${FALLBACK_THEME}'."
    plymouth-set-default-theme "${FALLBACK_THEME}" -R 2>/dev/null || true
    log "Fallback theme '${FALLBACK_THEME}' set."
    exit 1
  fi
fi

# ── Verify active theme matches ───────────────────────────────────────────────
ACTIVE=$(plymouth-set-default-theme 2>/dev/null || echo "")
if [[ "${ACTIVE}" != "${THEME}" ]]; then
  log "Active theme is '${ACTIVE}', setting to '${THEME}'..."
  plymouth-set-default-theme "${THEME}" -R
  ACTIVE=$(plymouth-set-default-theme 2>/dev/null || echo "")
fi

if [[ "${ACTIVE}" == "${THEME}" ]]; then
  log "Theme '${THEME}' is active and valid."
  exit 0
else
  warn "Failed to activate '${THEME}'. Falling back to '${FALLBACK_THEME}'."
  plymouth-set-default-theme "${FALLBACK_THEME}" -R 2>/dev/null || true
  exit 1
fi
