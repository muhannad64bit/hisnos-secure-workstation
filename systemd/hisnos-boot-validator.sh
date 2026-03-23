#!/bin/bash
# systemd/hisnos-boot-validator.sh
# Verifies critical boot pathways and constraints. Timeout is 15s.

log() { echo "HISNOS-BOOT-VALIDATOR: $*" | systemd-cat -t hisnos-boot-val; }

log "Running critical boot validation checks..."

# 1. Detect dangerous kernel params (unless in safe mode explicitly)
if ! grep -q "hisnos.mode=safe" /proc/cmdline; then
    for param in "ro" "single" "init=" "debug" "systemd.unit=rescue.target" "systemd.unit=emergency.target"; do
        if grep -q "\b$param\b" /proc/cmdline; then
            log "CRITICAL: Dangerous kernel param '$param' detected outside of safe mode!"
            log "Triggering panic fallback."
            systemctl isolate hisnos-safe.target
            exit 1
        fi
    done
fi

# 2. Verify overlay writable
if ! touch /run/hisnos-overlay-test; then
    log "CRITICAL: OverlayFS is not writable!"
    systemctl isolate hisnos-safe.target
    exit 1
fi
rm -f /run/hisnos-overlay-test

# 3. Verify dbus
if ! systemctl is-active --quiet dbus.service; then
    log "CRITICAL: dbus is not active!"
    systemctl isolate hisnos-safe.target
    exit 1
fi

# 4. Verify display manager
# Wait up to 5 seconds for it to register if not active yet
for i in {1..5}; do
    if systemctl is-enabled --quiet display-manager.service >/dev/null 2>&1 || [ -L /etc/systemd/system/display-manager.service ]; then
        log "Display manager is registered."
        break
    fi
    sleep 1
    if [ "$i" -eq 5 ]; then
        log "CRITICAL: display-manager not found in critical path!"
        systemctl isolate hisnos-safe.target
        exit 1
    fi
done

log "Boot validation passed successfully."
exit 0
