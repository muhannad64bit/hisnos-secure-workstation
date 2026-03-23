#!/bin/bash
# systemd/hisnos-stability-guard.sh
# Runs every 6h to check long-term stability and disk health

log() { echo "HISNOS-STABILITY-GUARD: $*" | systemd-cat -t hisnos-stability-guard; }

log "Running comprehensive 6-hour stability check."

# journal runaway check (500MB limit)
if [ -d /var/log/journal ]; then
    JOURNAL_SIZE=$(du -sm /var/log/journal | awk '{print $1}')
    if [ "$JOURNAL_SIZE" -gt 500 ]; then
        log "WARNING: Journal size > 500MB. Pruning to 200MB."
        journalctl --vacuum-size=200M
    fi
fi

# disk >90% check
DISK_USAGE=$(df / | awk 'NR==2 {print $5}' | sed 's/%//')
if [ "$DISK_USAGE" -gt 90 ]; then
    log "CRITICAL: Root filesystem usage at ${DISK_USAGE}%. Escalating to Safe Mode."
    systemctl isolate hisnos-safe.target
fi

# zombie processes check (limit 50)
ZOMBIES=$(ps axo stat | grep -c Z)
if [ "$ZOMBIES" -gt 50 ]; then
    log "CRITICAL: Detected $ZOMBIES zombie processes. Kernel state unstable. Escalating."
    systemctl isolate hisnos-safe.target
fi

# OSTree integrity check
log "Running basic OSTree health check."
if ! ostree show local: >/dev/null 2>&1; then
    log "CRITICAL: OSTree repo integrity check failed! Escalating."
    systemctl isolate hisnos-safe.target
fi

# swap/memory pressure
SWAP_USED=$(free | awk '/Swap/{printf("%.0f"), $3/(($2>0)?$2:1)*100}')
if [ "$SWAP_USED" -gt 80 ]; then
    log "CRITICAL: Excessive swap usage detected (${SWAP_USED}%). Escalating to prevent IO storm."
    systemctl isolate hisnos-safe.target
fi

log "Stability checks passed."
