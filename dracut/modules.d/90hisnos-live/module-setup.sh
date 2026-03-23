#!/bin/bash
# dracut module descriptor — 90hisnos-live
#
# Provides deterministic live root mounting for HisnOS ISO boot:
#   ISO block device → squashfs loop mount → overlayfs → switch_root
#
# Trigger: kernel cmdline parameter  hisnos.live=1
# No dependency on inst.stage2 or Anaconda.

check() {
    # Return 255 = "include if explicitly requested OR if hisnos.live= is on
    # the build-time cmdline".  Return 0 = "always include".
    # We use 0 so the module is always built into HisnOS initramfs images.
    return 0
}

depends() {
    # We only need the dracut base.  Explicitly do NOT depend on 'live' to
    # avoid pulling in Fedora's dracut-live module which handles inst.stage2.
    echo "base"
    return 0
}

install() {
    # ── Shared library ─────────────────────────────────────────────────────
    inst_simple "$moddir/hisnos-live-lib.sh"  /lib/dracut/hisnos-live-lib.sh

    # ── Hook scripts (installed at their respective hook stages) ───────────
    #
    # cmdline/91  — parse hisnos.live=* parameters; set rootok=1
    inst_hook cmdline     91 "$moddir/hisnos-live-cmdline.sh"
    #
    # initqueue/30 — periodically try to detect the source device
    inst_hook initqueue   30 "$moddir/hisnos-live-initqueue.sh"
    #
    # initqueue/finished/30 — signal "source found" so dracut stops waiting
    inst_hook initqueue/finished 30 "$moddir/hisnos-live-finished.sh"
    #
    # mount/30 — the main mount logic (ISO → squashfs → overlayfs → NEWROOT)
    inst_hook mount       30 "$moddir/hisnos-live-root.sh"
    #
    # pre-pivot/91 — final validation before switch_root is called
    inst_hook pre-pivot   91 "$moddir/hisnos-live-validate.sh"

    # ── Emergency shell ────────────────────────────────────────────────────
    inst_simple "$moddir/hisnos-emergency.sh" /bin/hisnos-emergency
    chmod +x "$initdir/bin/hisnos-emergency"

    # ── systemd generator (installed into initrd; runs in live root) ───────
    inst_simple "$moddir/hisnos-live-generator.sh" \
        /usr/lib/systemd/system-generators/hisnos-live-generator
    chmod +x \
        "$initdir/usr/lib/systemd/system-generators/hisnos-live-generator"

    # ── Required binaries ──────────────────────────────────────────────────
    inst_multiple \
        mount umount losetup \
        blkid findmnt lsblk \
        mktemp \
        stty clear reset \
        grep awk sed cut sort head \
        sleep true false \
        cat printf echo \
        date

    # ── State directory ────────────────────────────────────────────────────
    inst_dir /run/hisnos

    return 0
}

installkernel() {
    # Filesystem drivers required for live boot.
    instmods squashfs overlay loop
    instmods iso9660 vfat
    # Block device drivers for USB sticks, optical drives.
    instmods usb-storage uas sd_mod sr_mod cdrom
    instmods dm_mod
}
