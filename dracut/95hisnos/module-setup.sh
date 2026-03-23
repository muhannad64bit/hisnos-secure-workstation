#!/usr/bin/env bash
# dracut/95hisnos/module-setup.sh
#
# Production dracut module for HisnOS Secure Workstation.
# Priority 95 — runs after base modules, before pivot.
#
# Provides:
#   1. Boot health pre-check (kernel cmdline validation)
#   2. Vault unlock policy enforcement
#   3. Safe-mode gate (hisnos.safemode=1 cmdline)
#   4. Emergency fallback escalation
#   5. Recovery menu (hisnos.recovery=1 cmdline)
#
# Install path: /usr/lib/dracut/modules.d/95hisnos/

check() {
    # Always include in HisnOS images
    return 0
}

depends() {
    echo "base systemd bash"
    return 0
}

installkernel() {
    # Pull in kernel modules needed for vault unlock + storage
    instmods \
        overlay \
        fuse \
        loop \
        squashfs \
        crypto_user \
        aes_x86_64 \
        aes_generic \
        cbc \
        sha256_generic \
        dm-crypt \
        dm-mod \
        vfat \
        nls_utf8 \
        || true
}

install() {
    # ── Boot hooks ───────────────────────────────────────────────────────────
    # pre-udev: kernel cmdline validation (very early)
    inst_hook pre-udev 05 "${moddir}/hisnos-cmdline-check.sh"
    # pre-pivot: main boot health + safe-mode gate
    inst_hook pre-pivot 40 "${moddir}/hisnos-boot.sh"
    # pre-pivot 50: recovery menu (only if hisnos.recovery=1)
    inst_hook pre-pivot 50 "${moddir}/hisnos-recovery-menu.sh"

    # ── Scripts ──────────────────────────────────────────────────────────────
    inst_simple "${moddir}/hisnos-cmdline-check.sh"
    inst_simple "${moddir}/hisnos-boot.sh"
    inst_simple "${moddir}/hisnos-recovery-menu.sh"
    inst_simple "${moddir}/hisnos-vault-unlock.sh"
    inst_simple "${moddir}/hisnos-lib.sh"

    # ── Required binaries ────────────────────────────────────────────────────
    inst_multiple \
        bash \
        sh \
        cat less grep awk sed tee cut sort uniq head tail \
        ls cp mv rm mkdir rmdir ln chmod chown touch \
        mount umount mountpoint findmnt \
        fsck e2fsck \
        blkid lsblk \
        cryptsetup \
        keyctl \
        logger \
        ip ss \
        ping \
        nft \
        systemctl \
        journalctl \
        uname \
        date \
        sleep \
        kill killall \
        ps \
        reboot poweroff \
        stty \
        tput \
        || true

    # ── gocryptfs for vault pre-unlock ───────────────────────────────────────
    inst_multiple \
        gocryptfs \
        fuse3 \
        || true

    # ── SSH rescue (dropbear) ────────────────────────────────────────────────
    inst_multiple \
        dropbear \
        dropbearkey \
        || true

    # ── Include HisnOS config files ──────────────────────────────────────────
    # Kernel cmdline policy
    inst_simple /etc/hisnos/cmdline-policy.conf 2>/dev/null || true
    # Release metadata
    inst_simple /etc/hisnos/release 2>/dev/null || true

    # ── /etc/resolv.conf for recovery DNS ───────────────────────────────────
    inst_simple /etc/resolv.conf 2>/dev/null || true

    # ── Locale for proper text rendering ────────────────────────────────────
    inst_simple /usr/lib/locale/locale-archive 2>/dev/null || true
}
