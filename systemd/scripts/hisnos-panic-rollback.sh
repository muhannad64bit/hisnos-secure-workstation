#!/bin/bash
# /usr/local/lib/hisnos/hisnos-panic-rollback.sh — Post-Panic OSTree Rollback
#
# Runs as hisnos-panic-rollback.service on boot.
# Checks if the previous boot was a panic (unclean shutdown).
# If so, marks the current deployment as bad and offers rollback.
#
# Detection: if /run/hisnos/clean-shutdown does NOT exist after boot,
# the previous shutdown was unclean (panic, power cut, etc.).
#
# Integration: hisnos-boot-complete.service creates the clean-shutdown marker.

set -uo pipefail

LOG_TAG="hisnos-panic-rollback"
MARKER="/var/lib/hisnos/.last-boot-clean"
PANIC_COUNT_FILE="/var/lib/hisnos/.panic-count"
PANIC_THRESHOLD=3

log_info() { logger -t "$LOG_TAG" -p daemon.info "$*"; echo "[panic-rollback] $*"; }
log_warn() { logger -t "$LOG_TAG" -p daemon.warning "$*"; echo "[panic-rollback] WARN: $*"; }
log_crit() { logger -t "$LOG_TAG" -p daemon.crit "$*"; echo "[panic-rollback] CRIT: $*"; }

mkdir -p /var/lib/hisnos 2>/dev/null || true

# Check if previous boot was clean
if [[ -f "$MARKER" ]]; then
    # Previous shutdown was clean — reset panic count
    log_info "Previous boot shutdown cleanly"
    echo "0" > "$PANIC_COUNT_FILE" 2>/dev/null || true
else
    # Unclean shutdown (panic, power cut, etc.)
    local_count=$(cat "$PANIC_COUNT_FILE" 2>/dev/null || echo "0")
    local_count=$((local_count + 1))
    echo "$local_count" > "$PANIC_COUNT_FILE"

    log_warn "Unclean shutdown detected (count: $local_count / $PANIC_THRESHOLD)"

    if [[ "$local_count" -ge "$PANIC_THRESHOLD" ]]; then
        log_crit "Panic threshold reached ($local_count) — attempting OSTree rollback"

        # Mark current deployment as bad in bootloader
        if command -v grub2-editenv &>/dev/null; then
            grub2-editenv - set "hisnos_bad_deploy=1" 2>/dev/null || true
        fi

        # Attempt rollback
        if command -v rpm-ostree &>/dev/null; then
            log_crit "Initiating rpm-ostree rollback"
            rpm-ostree rollback 2>/dev/null || {
                log_crit "rpm-ostree rollback failed — manual intervention required"
            }
        fi

        # Reset counter
        echo "0" > "$PANIC_COUNT_FILE"
    fi
fi

# Remove marker — it will be re-created by hisnos-boot-complete on clean shutdown
rm -f "$MARKER" 2>/dev/null || true

# Create clean-shutdown hook
# This marker is touched by ExecStop of this service on clean shutdown
log_info "Panic rollback check complete"
exit 0
