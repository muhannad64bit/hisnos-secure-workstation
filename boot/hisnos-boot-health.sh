#!/usr/bin/env bash
# boot/hisnos-boot-health.sh
#
# Writes /var/lib/hisnos/boot-health.json after each successful boot.
# Executed by hisnos-boot-health.service (After=multi-user.target).
#
# Fields written:
#   boot_timestamp       ISO-8601 UTC
#   boot_duration        seconds from kernel start (systemd UserSpaceSec)
#   failed_units_count   number of failed systemd units at check time
#   emergency_mode       true if emergency.target was active this boot
#   rescue_mode          true if rescue.target was active this boot
#   last_boot_successful true unless emergency/rescue/failures > 0
#   kernel_version       uname -r
#   warnings             list of detected problems

set -euo pipefail

STATE_DIR="/var/lib/hisnos"
HEALTH_FILE="${STATE_DIR}/boot-health.json"
LOG_FILE="/var/log/hisnos/boot-health.log"

mkdir -p "${STATE_DIR}" "$(dirname "${LOG_FILE}")"

log() { echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] $*" | tee -a "${LOG_FILE}"; }

log "hisnos-boot-health starting"

# ── Boot duration ─────────────────────────────────────────────────────────────
# systemd-analyze returns e.g. "Startup finished in 4.321s (kernel) + 12.456s (userspace)"
BOOT_DURATION="unknown"
if ANALYZE=$(systemd-analyze 2>/dev/null); then
  if [[ "${ANALYZE}" =~ ([0-9]+\.[0-9]+)s\ \(userspace\) ]]; then
    BOOT_DURATION="${BASH_REMATCH[1]}"
  fi
fi

# ── Failed units ──────────────────────────────────────────────────────────────
FAILED_UNITS=()
while IFS= read -r unit; do
  [[ -n "${unit}" ]] && FAILED_UNITS+=("${unit}")
done < <(systemctl list-units --state=failed --no-legend --no-pager --plain 2>/dev/null \
         | awk '{print $1}')

FAILED_COUNT="${#FAILED_UNITS[@]}"

# ── Emergency / rescue mode detection ────────────────────────────────────────
EMERGENCY="false"
RESCUE="false"

# Check the kernel cmdline for systemd.unit= override used by recovery.
if grep -q 'systemd\.unit=emergency\.target' /proc/cmdline 2>/dev/null; then
  EMERGENCY="true"
fi
if grep -q 'systemd\.unit=rescue\.target\|hisnos\.recovery=1' /proc/cmdline 2>/dev/null; then
  RESCUE="true"
fi
# Also check if emergency.target is (was) active.
if systemctl is-active emergency.target &>/dev/null; then
  EMERGENCY="true"
fi

# ── Warnings list ─────────────────────────────────────────────────────────────
WARNINGS="[]"
warn_list=()

for u in "${FAILED_UNITS[@]}"; do
  warn_list+=("\"failed unit: ${u}\"")
done

if [[ "${EMERGENCY}" == "true" ]]; then
  warn_list+=("\"emergency.target was active this boot\"")
fi
if [[ "${RESCUE}" == "true" ]]; then
  warn_list+=("\"rescue/recovery mode was active this boot\"")
fi

if [[ "${#warn_list[@]}" -gt 0 ]]; then
  WARNINGS="[$(IFS=,; echo "${warn_list[*]}")]"
fi

# ── Overall success flag ──────────────────────────────────────────────────────
LAST_SUCCESS="true"
if [[ "${EMERGENCY}" == "true" || "${RESCUE}" == "true" || "${FAILED_COUNT}" -gt 0 ]]; then
  LAST_SUCCESS="false"
fi

# ── Write JSON ────────────────────────────────────────────────────────────────
KERNEL_VER=$(uname -r)
TIMESTAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ)

TMP_FILE=$(mktemp "${STATE_DIR}/.boot-health-XXXXXX.tmp")
cat > "${TMP_FILE}" << JSONEOF
{
  "boot_timestamp": "${TIMESTAMP}",
  "boot_duration": "${BOOT_DURATION}",
  "failed_units_count": ${FAILED_COUNT},
  "emergency_mode": ${EMERGENCY},
  "rescue_mode": ${RESCUE},
  "last_boot_successful": ${LAST_SUCCESS},
  "kernel_version": "${KERNEL_VER}",
  "warnings": ${WARNINGS}
}
JSONEOF

mv "${TMP_FILE}" "${HEALTH_FILE}"
chmod 0644 "${HEALTH_FILE}"

log "boot-health written: success=${LAST_SUCCESS} duration=${BOOT_DURATION}s failed=${FAILED_COUNT}"

# ── If emergency mode: flag the system state file ─────────────────────────────
CORE_STATE="/var/lib/hisnos/core-state.json"
if [[ "${EMERGENCY}" == "true" ]] && [[ -f "${CORE_STATE}" ]]; then
  # Inject emergency flag into core-state.json using a simple sed in-place.
  # Proper JSON manipulation would require jq; use sed as fallback (jq preferred).
  if command -v jq &>/dev/null; then
    TMP_STATE=$(mktemp "${STATE_DIR}/.core-state-XXXXXX.tmp")
    jq '.last_emergency_boot = true | .mode = "safe-mode"' "${CORE_STATE}" > "${TMP_STATE}"
    mv "${TMP_STATE}" "${CORE_STATE}"
    log "core-state.json flagged: mode=safe-mode (emergency boot detected)"
  fi
fi

exit 0
