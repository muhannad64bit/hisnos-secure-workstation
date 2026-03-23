#!/usr/bin/env bash
# recovery/dracut/95hisnos-recovery/module-setup.sh
#
# Dracut module descriptor for 95hisnos-recovery.
# Installs the pre-pivot recovery hook into the initramfs.

check() {
    return 0
}

depends() {
    echo "bash"
    return 0
}

install() {
    # Install the recovery hook at priority 50 within pre-pivot.
    inst_hook pre-pivot 50 "${moddir}/hisnos-recovery.sh"

    # ── Core repair tools ──────────────────────────────────────────────────────
    inst_multiple \
        bash \
        ls cat less grep awk sed tee \
        mount umount mountpoint findmnt \
        fsck e2fsck xfs_repair btrfsck \
        blkid lsblk gdisk parted \
        cryptsetup \
        || true

    # ── Storage / vault ────────────────────────────────────────────────────────
    inst_multiple \
        gocryptfs \
        || true

    # ── Network tools ──────────────────────────────────────────────────────────
    inst_multiple \
        ip ss ping ping6 \
        nft \
        || true

    # ── Systemd / journal tools ───────────────────────────────────────────────
    inst_multiple \
        systemctl journalctl \
        || true

    # ── Diagnostics ───────────────────────────────────────────────────────────
    inst_multiple \
        strace \
        df du ps kill \
        reboot poweroff \
        || true

    # ── Optional: SSH rescue via dropbear ─────────────────────────────────────
    inst_multiple \
        dropbear dropbearkey \
        || true   # non-fatal — SSH menu option detects absence at runtime

    # ── Include /etc/resolv.conf for DNS in recovery ──────────────────────────
    inst_simple /etc/resolv.conf 2>/dev/null || true
}
