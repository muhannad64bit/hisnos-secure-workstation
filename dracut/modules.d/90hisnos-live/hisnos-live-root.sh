#!/bin/bash
# dracut hook: mount/30-hisnos-live-root.sh
#
# HisnOS Live Root Mount — the core of the live boot system.
#
# Mount sequence:
#   1. Read cached source device path (/run/hisnos/source-dev)
#   2. Mount ISO device → /run/hisnos/isodev   (ro, iso9660)
#   3. Verify /LiveOS/rootfs.img exists
#   4. Loop-mount squashfs  → /run/hisnos/squashfs  (ro, squashfs)
#   5. Create overlay tmpfs → /run/hisnos/overlay    (rw, RAM-sized)
#   6. Mount overlayfs      → $NEWROOT               (rw merged)
#   7. Write live-ok marker → $NEWROOT/etc/hisnos/.live-ok
#   8. Copy boot log        → $NEWROOT/var/log/hisnos/
#
# On ANY failure: calls hisnos_die → launches emergency UI, never hangs.

# Only run in live mode.
getargbool 0 hisnos.live || exit 0

# Guard: don't remount if sysroot already has something.
findmnt -n "${NEWROOT}" &>/dev/null && {
    echo "hisnos-live: mount hook: ${NEWROOT} already mounted, skipping" > /dev/kmsg 2>/dev/null || true
    exit 0
}

. /lib/dracut/hisnos-live-lib.sh

hisnos_init_dirs

hisnos_log INFO "════════════════════════════════════════════"
hisnos_log INFO " HisnOS Live Root Mount"
hisnos_log INFO "════════════════════════════════════════════"

# ── Step 1: Read cached source device ────────────────────────────────────
hisnos_log INFO "[1/7] Reading source device..."
if [[ ! -f /run/hisnos/source-dev ]]; then
    # initqueue should have populated this; try once more.
    hisnos_log WARN "source-dev not cached — running detection now"
    if ! hisnos_find_source_dev; then
        hisnos_die \
            "No HisnOS ISO source device found.\n" \
            "Tried: cmdline override, CDLABEL=${HISNOS_CDLABEL}, blkid scan, device scan.\n" \
            "Is the USB/DVD inserted and readable?"
    fi
    echo "${HISNOS_SOURCE_DEV}" > /run/hisnos/source-dev
else
    HISNOS_SOURCE_DEV=$(cat /run/hisnos/source-dev)
    [[ -b "${HISNOS_SOURCE_DEV}" ]] || \
        hisnos_die "Cached source device ${HISNOS_SOURCE_DEV} is no longer a block device."
fi
hisnos_log OK "Source device: ${HISNOS_SOURCE_DEV}"

# ── Step 2: Mount ISO device ──────────────────────────────────────────────
hisnos_log INFO "[2/7] Mounting ISO device (read-only)..."
if ! mount -o ro,noatime "${HISNOS_SOURCE_DEV}" "${HISNOS_ISO_MNT}" 2>/tmp/mnt-err; then
    ERR=$(<"/tmp/mnt-err")
    hisnos_die "Failed to mount ISO device ${HISNOS_SOURCE_DEV}: ${ERR}"
fi
hisnos_log OK "ISO mounted: ${HISNOS_SOURCE_DEV} → ${HISNOS_ISO_MNT}"

# ── Step 3: Verify squashfs image ─────────────────────────────────────────
hisnos_log INFO "[3/7] Locating squashfs image..."
SQUASHFS_IMG="${HISNOS_ISO_MNT}${HISNOS_LIVE_IMG}"
if [[ ! -f "${SQUASHFS_IMG}" ]]; then
    # Try alternate path for older builds.
    ALT="${HISNOS_ISO_MNT}/LiveOS/squashfs.img"
    if [[ -f "${ALT}" ]]; then
        SQUASHFS_IMG="${ALT}"
        hisnos_log WARN "Using alternate squashfs path: ${ALT}"
    else
        umount "${HISNOS_ISO_MNT}" 2>/dev/null
        hisnos_die \
            "Squashfs image not found.\n" \
            "Expected: ${SQUASHFS_IMG}\n" \
            "ISO may be corrupted or built with a different lorax template."
    fi
fi
SQUASHFS_SIZE=$(du -sh "${SQUASHFS_IMG}" 2>/dev/null | cut -f1 || echo "?")
hisnos_log OK "Squashfs: ${SQUASHFS_IMG} (${SQUASHFS_SIZE})"

# ── Step 4: Loop-mount squashfs ───────────────────────────────────────────
hisnos_log INFO "[4/7] Loop-mounting squashfs (read-only)..."
LOOP_DEV=$(losetup -f --show --read-only "${SQUASHFS_IMG}" 2>/tmp/loop-err)
if [[ -z "${LOOP_DEV}" ]]; then
    ERR=$(<"/tmp/loop-err")
    umount "${HISNOS_ISO_MNT}" 2>/dev/null
    hisnos_die "losetup failed for ${SQUASHFS_IMG}: ${ERR}"
fi
echo "${LOOP_DEV}" > /run/hisnos/loop-dev
hisnos_log INFO "Loop device: ${LOOP_DEV}"

if ! mount -o ro -t squashfs "${LOOP_DEV}" "${HISNOS_SQUASHFS_MNT}" 2>/tmp/sq-err; then
    ERR=$(<"/tmp/sq-err")
    losetup -d "${LOOP_DEV}" 2>/dev/null
    umount "${HISNOS_ISO_MNT}" 2>/dev/null
    hisnos_die "squashfs mount failed (${LOOP_DEV}): ${ERR}"
fi
hisnos_log OK "Squashfs mounted: ${LOOP_DEV} → ${HISNOS_SQUASHFS_MNT}"

# Verify the squashfs contains a usable OS tree.
if ! hisnos_validate_root "${HISNOS_SQUASHFS_MNT}"; then
    umount "${HISNOS_SQUASHFS_MNT}" 2>/dev/null
    losetup -d "${LOOP_DEV}" 2>/dev/null
    umount "${HISNOS_ISO_MNT}" 2>/dev/null
    hisnos_die "Squashfs does not contain a valid OS root (missing usr/etc/bin)."
fi

# ── Step 5: Create overlay tmpfs ─────────────────────────────────────────
hisnos_log INFO "[5/7] Creating overlay tmpfs..."
OVERLAY_SIZE=$(hisnos_overlay_size)
hisnos_log INFO "Overlay size: ${OVERLAY_SIZE} (50% of RAM, capped 512M–4G)"

if ! mount -t tmpfs -o "size=${OVERLAY_SIZE},mode=0755" tmpfs "${HISNOS_OVERLAY_DIR}" 2>/tmp/tmpfs-err; then
    ERR=$(<"/tmp/tmpfs-err")
    umount "${HISNOS_SQUASHFS_MNT}" 2>/dev/null
    losetup -d "${LOOP_DEV}" 2>/dev/null
    umount "${HISNOS_ISO_MNT}" 2>/dev/null
    hisnos_die "tmpfs creation failed (size=${OVERLAY_SIZE}): ${ERR}"
fi
mkdir -p "${HISNOS_OVERLAY_DIR}/upper" "${HISNOS_OVERLAY_DIR}/work"
hisnos_log OK "Overlay tmpfs: ${HISNOS_OVERLAY_DIR} (${OVERLAY_SIZE})"

# ── Step 6: Mount overlayfs → $NEWROOT ────────────────────────────────────
hisnos_log INFO "[6/7] Mounting overlayfs → ${NEWROOT}..."
mkdir -p "${NEWROOT}"

OVERLAY_OPTS="lowerdir=${HISNOS_SQUASHFS_MNT},upperdir=${HISNOS_OVERLAY_DIR}/upper,workdir=${HISNOS_OVERLAY_DIR}/work"
if ! mount -t overlay overlay -o "${OVERLAY_OPTS}" "${NEWROOT}" 2>/tmp/ov-err; then
    ERR=$(<"/tmp/ov-err")
    umount "${HISNOS_OVERLAY_DIR}" 2>/dev/null
    umount "${HISNOS_SQUASHFS_MNT}" 2>/dev/null
    losetup -d "${LOOP_DEV}" 2>/dev/null
    umount "${HISNOS_ISO_MNT}" 2>/dev/null
    hisnos_die \
        "overlayfs mount failed.\n" \
        "Options: ${OVERLAY_OPTS}\n" \
        "Error: ${ERR}\n" \
        "Kernel may lack CONFIG_OVERLAY_FS=y."
fi
hisnos_log OK "overlayfs mounted → ${NEWROOT}"

# ── Step 7: Write live-ok marker + boot state ─────────────────────────────
hisnos_log INFO "[7/7] Writing live-ok marker and boot state..."

# /etc/hisnos/.live-ok survives into the live system (overlayfs upper tier).
# The hisnos-live-generator systemd generator reads it to create
# /run/hisnos-live-ok after systemd mounts its own /run tmpfs.
mkdir -p "${NEWROOT}/etc/hisnos" 2>/dev/null || true
touch "${NEWROOT}/etc/hisnos/.live-ok" 2>/dev/null || \
    hisnos_log WARN "Could not write ${NEWROOT}/etc/hisnos/.live-ok"

# Write full boot state (read by hisnos-live-health.service).
mkdir -p "${NEWROOT}/etc/hisnos" 2>/dev/null || true
cat > "${NEWROOT}/etc/hisnos/live-boot-state" 2>/dev/null <<_STATE
HISNOS_LIVE=1
HISNOS_SOURCE_DEV=${HISNOS_SOURCE_DEV}
HISNOS_LOOP_DEV=${LOOP_DEV}
HISNOS_SQUASHFS_IMG=${SQUASHFS_IMG}
HISNOS_OVERLAY_SIZE=${OVERLAY_SIZE}
HISNOS_OVERLAY_DIR=${HISNOS_OVERLAY_DIR}
BOOT_TIMESTAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)
_STATE

# Copy dracut boot log into the live root for post-boot diagnostics.
mkdir -p "${NEWROOT}/var/log/hisnos" 2>/dev/null || true
cp "${HISNOS_LOG}" "${NEWROOT}/var/log/hisnos/dracut-live-boot.log" 2>/dev/null || true

hisnos_log OK "════════════════════════════════════════════"
hisnos_log OK " Live root mount COMPLETE"
hisnos_log OK "════════════════════════════════════════════"
exit 0
