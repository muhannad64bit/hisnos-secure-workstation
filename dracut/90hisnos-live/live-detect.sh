#!/bin/bash
# 90hisnos-live/live-detect.sh

log() { echo "HISNOS-LIVE: $*" | systemd-cat -t hisnos-live; }

# Redirect output to console for immediate visibility if systemd isn't absorbing it yet
echo "HisnOS-Live: Detecting ISO device..."

# Wait up to 10 seconds for device to appear
TIMEOUT=10
LIVE_DEV=""

for ((i=0; i<TIMEOUT; i++)); do
    udevadm settle
    for dev in $(lsblk -lno PATH,FSTYPE | grep -E 'iso9660|vfat|ext4' | awk '{print $1}'); do
        if blkid "$dev" | grep -q 'LABEL="HisnOS-Live"'; then
            LIVE_DEV="$dev"
            break 2
        fi
    done
    sleep 1
done

if [ -z "$LIVE_DEV" ]; then
    log "CRITICAL: Could not detect HisnOS-Live ISO device!"
    /sbin/emergency-ui "Missing Live Device"
    exit 1
fi

log "Detected HisnOS-Live device at $LIVE_DEV"
echo "$LIVE_DEV" > /tmp/hisnos-live-dev
