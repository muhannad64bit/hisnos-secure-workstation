#!/usr/bin/env bash
# lab/runtime/hisnos-lab-stop.sh — Stop the active lab session
#
# Usage:
#   hisnos-lab-stop.sh --session-id <id>
#   hisnos-lab-stop.sh --force    # stop whatever is in the lock file
#
# Exit codes:
#   0  — session stopped cleanly
#   1  — argument error
#   2  — no active session found
#   3  — systemctl stop failed (unit may already be dead)
#
# This script is called by the dashboard API (POST /api/lab/stop).
# It is also safe to run manually for emergency cleanup.
# The runtime script's EXIT trap handles lock file removal independently;
# this script issues the stop signal and waits for confirmation.

set -euo pipefail

readonly SYSTEMCTL_BIN="/usr/bin/systemctl"
readonly LOGGER_BIN="/usr/bin/logger"
readonly LAB_UNIT_PREFIX="hisnos-lab-"
readonly SESSION_FILE="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/hisnos-lab-session.json"

SESSION_ID=""
FORCE=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --session-id) SESSION_ID="$2"; shift 2 ;;
    --force)      FORCE=true;      shift   ;;
    *)
      echo "[hisnos-lab-stop] unknown argument: $1" >&2
      exit 1
      ;;
  esac
done

# ── Resolve session ───────────────────────────────────────────────────────────

if [[ "${FORCE}" == "true" ]]; then
  if [[ ! -f "${SESSION_FILE}" ]]; then
    echo "[hisnos-lab-stop] no active session (lock file missing)" >&2
    exit 2
  fi
  # Read session ID from lock file
  if command -v python3 &>/dev/null; then
    SESSION_ID="$(python3 -c "import json,sys; d=json.load(open('${SESSION_FILE}')); print(d['session_id'])" 2>/dev/null || true)"
  fi
  if [[ -z "${SESSION_ID}" ]]; then
    # Fallback: grep for session_id field
    SESSION_ID="$(grep -o '"session_id":"[^"]*"' "${SESSION_FILE}" | cut -d'"' -f4 || true)"
  fi
  if [[ -z "${SESSION_ID}" ]]; then
    echo "[hisnos-lab-stop] could not read session_id from lock file" >&2
    exit 2
  fi
elif [[ -z "${SESSION_ID}" ]]; then
  echo "[hisnos-lab-stop] --session-id or --force is required" >&2
  exit 1
fi

UNIT_NAME="${LAB_UNIT_PREFIX}${SESSION_ID}.service"

# ── Stop the unit ─────────────────────────────────────────────────────────────

echo "[hisnos-lab-stop] stopping unit: ${UNIT_NAME}"

# Check if unit is actually active before attempting stop
if ! "${SYSTEMCTL_BIN}" --user is-active --quiet "${UNIT_NAME}" 2>/dev/null; then
  echo "[hisnos-lab-stop] unit ${UNIT_NAME} is not active — cleaning up stale lock file"
  rm -f "${SESSION_FILE}" 2>/dev/null || true
  "${LOGGER_BIN}" -t hisnos-lab -p user.notice \
    "HISNOS_LAB_STOPPED session=${SESSION_ID} reason=already_inactive" \
    2>/dev/null || true
  exit 0
fi

if ! "${SYSTEMCTL_BIN}" --user stop "${UNIT_NAME}" 2>/dev/null; then
  # Unit may have exited between the is-active check and the stop call.
  # Treat as already stopped.
  echo "[hisnos-lab-stop] stop command returned non-zero (unit may have already exited)" >&2
fi

# ── Verify stopped ────────────────────────────────────────────────────────────

local_wait=0
while "${SYSTEMCTL_BIN}" --user is-active --quiet "${UNIT_NAME}" 2>/dev/null; do
  if [[ ${local_wait} -ge 50 ]]; then
    echo "[hisnos-lab-stop] WARNING: unit still active after 5s" >&2
    break
  fi
  sleep 0.1
  (( local_wait++ ))
done

# ── Clean up lock file ────────────────────────────────────────────────────────
# The runtime script's EXIT trap should have already removed it, but we
# ensure removal here as a belt+suspenders measure.

rm -f "${SESSION_FILE}" 2>/dev/null || true

"${LOGGER_BIN}" -t hisnos-lab -p user.notice \
  "HISNOS_LAB_STOPPED session=${SESSION_ID} reason=operator_stop" \
  2>/dev/null || true

echo "[hisnos-lab-stop] session ${SESSION_ID} stopped"
exit 0
