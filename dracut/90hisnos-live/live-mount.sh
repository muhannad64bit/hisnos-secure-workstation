#!/bin/bash
# 90hisnos-live/live-mount.sh

log() { echo "HISNOS-LIVE: $*" | systemd-cat -t hisnos-live; }

LIVE_DEV=$(cat /tmp/hisnos-live-dev)
ISO_MOUNT="/run/hisnos/iso"
SQFS_MOUNT="/run/hisnos/squashfs"

mkdir -p "$ISO_MOUNT" "$SQFS_MOUNT"

log "Mounting ISO from $LIVE_DEV..."
if ! mount -o ro "$LIVE_DEV" "$ISO_MOUNT"; then
    log "CRITICAL: Failed to mount ISO device: $LIVE_DEV"
    /sbin/emergency-ui "ISO Mount Failure"
    exit 1
fi

SQFS_FILE="$ISO_MOUNT/LiveOS/squashfs.img"
if [ ! -f "$SQFS_FILE" ]; then
    log "CRITICAL: SquashFS image not found at $SQFS_FILE!"
    /sbin/emergency-ui "Missing SquashFS"
    exit 1
fi

log "Mounting SquashFS from $SQFS_FILE..."
if ! mount -o ro,loop "$SQFS_FILE" "$SQFS_MOUNT"; then
    log "CRITICAL: Failed to mount SquashFS image!"
    /sbin/emergency-ui "SquashFS Mount Failure"
    exit 1
fi

log "Successfully mounted ISO and SquashFS."
