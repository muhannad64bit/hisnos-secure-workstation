#!/bin/bash
# systemd/hisnos-watchdog.sh
# Real-time monitoring for catastrophic failures

log() { echo "HISNOS-WATCHDOG: $*" | systemd-cat -t hisnos-watchdog; }

log "Watchdog started."

# Ensure watchdog touches systemd to satisfy WatchdogSec
trap 'log "Watchdog stopped"; exit 0' SIGTERM SIGINT

while true; do
    # Memory Pressure > 90%
    MEM_USED=$(free | awk '/Mem/{printf("%.0f"), $3/$2*100}')
    if [ "$MEM_USED" -gt 90 ]; then
        log "WARNING: Memory pressure at ${MEM_USED}%"
        sync; echo 1 > /proc/sys/vm/drop_caches
        sleep 5
        MEM_USED=$(free | awk '/Mem/{printf("%.0f"), $3/$2*100}')
        if [ "$MEM_USED" -gt 90 ]; then
            log "CRITICAL: Memory pressure sustained at ${MEM_USED}%. Escalating to Safe Mode."
            systemctl isolate hisnos-safe.target
        fi
    fi

    # Check for rpm-ostree stuck > 30min (1800s)
    if pgrep rpm-ostree >/dev/null; then
        OSTREE_TIME=$(ps -o etimes= -p $(pgrep -n rpm-ostree) 2>/dev/null || echo 0)
        if [ "$OSTREE_TIME" -gt 1800 ]; then
            log "CRITICAL: rpm-ostree stuck for > 30 mins. Escalating to Safe Mode."
            killall -9 rpm-ostree
            systemctl isolate hisnos-safe.target
        fi
    fi

    # Systemd notify ping
    systemd-notify WATCHDOG=1 2>/dev/null || true
    
    # Run every 10 seconds
    sleep 10
done
