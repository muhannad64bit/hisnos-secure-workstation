#!/usr/bin/env bash
# installer/calamares/hisnos-precheck.sh
#
# Pre-install validation for HisnOS.
# Called by Calamares shellprocess-hisnos-precheck module BEFORE partitioning.
#
# Checks:
#   1) RAM >= 4 GB
#   2) Target disk >= 30 GB (largest unpartitioned block or selected target)
#   3) EFI partition / EFI boot mode
#   4) SecureBoot state (warn only — not a hard failure)
#
# Exit codes:
#   0 — all hard requirements met
#   1 — hard failure (Calamares will show failure page)
#
# Writes results to /tmp/hisnos-precheck-result.json for Calamares QML to display.

set -euo pipefail

LOG="/var/log/hisnos-install.log"
RESULT="/tmp/hisnos-precheck-result.json"

mkdir -p "$(dirname "${LOG}")"

log() { echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] [precheck] $*" | tee -a "${LOG}"; }

ERRORS=()
WARNINGS=()

# ── 1. RAM check ──────────────────────────────────────────────────────────────
RAM_KB=$(grep MemTotal /proc/meminfo | awk '{print $2}')
RAM_GB=$(( RAM_KB / 1024 / 1024 ))
MIN_RAM_GB=4

log "RAM detected: ${RAM_GB} GB (minimum: ${MIN_RAM_GB} GB)"
if [[ "${RAM_GB}" -lt "${MIN_RAM_GB}" ]]; then
  ERRORS+=("Insufficient RAM: ${RAM_GB} GB detected, ${MIN_RAM_GB} GB required")
else
  log "RAM check: PASS"
fi

# ── 2. Disk size check ────────────────────────────────────────────────────────
MIN_DISK_GB=30
LARGEST_DISK_GB=0
LARGEST_DISK=""

while IFS= read -r line; do
  DEV=$(echo "${line}" | awk '{print $1}')
  SIZE_BYTES=$(blockdev --getsize64 "/dev/${DEV}" 2>/dev/null || echo 0)
  SIZE_GB=$(( SIZE_BYTES / 1024 / 1024 / 1024 ))
  if [[ "${SIZE_GB}" -gt "${LARGEST_DISK_GB}" ]]; then
    LARGEST_DISK_GB="${SIZE_GB}"
    LARGEST_DISK="${DEV}"
  fi
done < <(lsblk -dno NAME,TYPE 2>/dev/null | awk '$2=="disk"{print $1}')

log "Largest disk: ${LARGEST_DISK} (${LARGEST_DISK_GB} GB, minimum: ${MIN_DISK_GB} GB)"
if [[ "${LARGEST_DISK_GB}" -lt "${MIN_DISK_GB}" ]]; then
  ERRORS+=("Insufficient disk: largest disk is ${LARGEST_DISK_GB} GB, ${MIN_DISK_GB} GB required")
else
  log "Disk check: PASS"
fi

# ── 3. EFI / boot mode ────────────────────────────────────────────────────────
EFI_BOOT="false"
EFI_PARTITION="false"

if [[ -d /sys/firmware/efi ]]; then
  EFI_BOOT="true"
  log "Boot mode: UEFI"
else
  log "Boot mode: Legacy BIOS"
  WARNINGS+=("System is in legacy BIOS mode. HisnOS is optimized for UEFI. Install will proceed but Secure Boot will not be available.")
fi

# Check for an existing EFI partition.
if lsblk -o PARTTYPE 2>/dev/null | grep -qi "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"; then
  EFI_PARTITION="true"
  log "EFI partition: found"
elif [[ "${EFI_BOOT}" == "true" ]]; then
  WARNINGS+=("No EFI system partition found. Calamares will create one automatically.")
  log "EFI partition: not found (will be created)"
fi

# ── 4. Secure Boot state (warn only) ─────────────────────────────────────────
SECURE_BOOT="unknown"
SB_FILE="/sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c"
if [[ -f "${SB_FILE}" ]]; then
  SB_VAL=$(od -An -tu1 "${SB_FILE}" 2>/dev/null | awk '{print $NF}' | tail -1)
  if [[ "${SB_VAL}" == "1" ]]; then
    SECURE_BOOT="enabled"
    WARNINGS+=("Secure Boot is enabled. If the HisnOS kernel is not signed, enroll the MOK or disable Secure Boot in firmware.")
    log "Secure Boot: ENABLED (warning)"
  else
    SECURE_BOOT="disabled"
    log "Secure Boot: disabled"
  fi
else
  log "Secure Boot: cannot detect (non-EFI or restricted access)"
fi

# ── Result JSON ───────────────────────────────────────────────────────────────
PASS=$( [[ "${#ERRORS[@]}" -eq 0 ]] && echo "true" || echo "false" )

# Build JSON arrays.
errors_json="["
for i in "${!ERRORS[@]}"; do
  [[ $i -gt 0 ]] && errors_json+=","
  errors_json+="\"${ERRORS[$i]}\""
done
errors_json+="]"

warnings_json="["
for i in "${!WARNINGS[@]}"; do
  [[ $i -gt 0 ]] && warnings_json+=","
  warnings_json+="\"${WARNINGS[$i]}\""
done
warnings_json+="]"

cat > "${RESULT}" << JSONEOF
{
  "pass": ${PASS},
  "ram_gb": ${RAM_GB},
  "disk_gb": ${LARGEST_DISK_GB},
  "disk_device": "${LARGEST_DISK}",
  "efi_boot": ${EFI_BOOT},
  "efi_partition": ${EFI_PARTITION},
  "secure_boot": "${SECURE_BOOT}",
  "errors": ${errors_json},
  "warnings": ${warnings_json}
}
JSONEOF

log "Pre-check result: pass=${PASS} errors=${#ERRORS[@]} warnings=${#WARNINGS[@]}"
cat "${RESULT}"

if [[ "${#ERRORS[@]}" -gt 0 ]]; then
  log "HARD FAILURE — aborting install"
  for e in "${ERRORS[@]}"; do log "  ERROR: ${e}"; done
  exit 1
fi

exit 0
