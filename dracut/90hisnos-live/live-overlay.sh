#!/bin/bash
# 90hisnos-live/live-overlay.sh

log() { echo "HISNOS-LIVE: $*" | systemd-cat -t hisnos-live; }

SQFS_MOUNT="/run/hisnos/squashfs"
ROOT_MOUNT="/sysroot"
OVERLAY_TMP="/run/hisnos/overlay"

log "Setting up Live OverlayFS..."

mkdir -p "$OVERLAY_TMP"
mount -t tmpfs -o mode=0755 tmpfs "$OVERLAY_TMP"
mkdir -p "$OVERLAY_TMP/upper" "$OVERLAY_TMP/work"

if ! mount -t overlay overlay -o lowerdir="$SQFS_MOUNT",upperdir="$OVERLAY_TMP/upper",workdir="$OVERLAY_TMP/work" "$ROOT_MOUNT"; then
    log "CRITICAL: Failed to create overlayfs on $ROOT_MOUNT!"
    /sbin/emergency-ui "OverlayFS Failure"
    exit 1
fi

log "OverlayFS successfully mounted at $ROOT_MOUNT"
