#!/bin/bash
# 90hisnos-live/module-setup.sh
# Core dracut module setup for HisnOS live boot

check() {
    # Always include this module as it's the core boot script
    return 0
}

depends() {
    echo "bash udev-rules rootfs-block fs-lib systemd"
    return 0
}

install() {
    inst_multiple \
        bash blkid lsblk mount umount grep awk sed cat tail \
        cut readlink dmesg journalctl systemctl systemd-cat sleep clear dialog

    inst_script "$moddir/live-detect.sh" "/sbin/live-detect"
    inst_script "$moddir/live-validate.sh" "/sbin/live-validate"
    inst_script "$moddir/emergency-ui.sh" "/sbin/emergency-ui"
    inst_script "$moddir/live-mount.sh" "/sbin/live-mount"
    inst_script "$moddir/live-overlay.sh" "/sbin/live-overlay"

    # Hook the dracut pre-mount phase (runs before standard mount)
    inst_hook pre-mount 10 "$moddir/live-detect.sh"
    inst_hook pre-mount 20 "$moddir/live-validate.sh"
    inst_hook pre-mount 30 "$moddir/live-mount.sh"
    inst_hook pre-mount 40 "$moddir/live-overlay.sh"

    # Ensure required rules are present
    inst_rules 60-cdrom_id.rules
}
