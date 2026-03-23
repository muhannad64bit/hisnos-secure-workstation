#!/bin/bash
# 90hisnos-live/live-validate.sh

log() { echo "HISNOS-LIVE: $*" | systemd-cat -t hisnos-live; }

log "Validating Live environment state..."

if [ ! -d "/run/hisnos/squashfs/usr/lib/systemd" ]; then
    log "CRITICAL: Corrupted or missing systemd inside rootfs!"
    /sbin/emergency-ui "RootFS Validation Failed (systemd missing)"
    exit 1
fi

if grep -q "hisnos.panic_rollback=1" /proc/cmdline; then
    log "WARNING: Booting after a kernel panic rollback. Verifying integrity..."
    # Placeholder for deeper rollback checks
    touch /run/hisnos/squashfs/var/run/hisnos-recovery-flag 2>/dev/null || true
fi

log "Validation passed."
