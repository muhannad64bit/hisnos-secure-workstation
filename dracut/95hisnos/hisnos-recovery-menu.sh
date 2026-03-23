#!/usr/bin/env bash
# dracut/95hisnos/hisnos-recovery-menu.sh
# Hook: pre-pivot priority 50
#
# Full interactive recovery environment. Activates ONLY when
# hisnos.recovery=1 is present on the kernel command line.
# After this script exits, systemd continues into rescue.target.

. /hisnos-lib.sh 2>/dev/null || true

# ─── Guard ───────────────────────────────────────────────────────────────────
hisnos_flag isset recovery || exit 0

SYSROOT="${NEWROOT:-/sysroot}"

# ─── Banner ───────────────────────────────────────────────────────────────────
clear 2>/dev/null || true
echo ""
echo -e "${CYAN}${BOLD}╔══════════════════════════════════════════════════════════╗${RESET}"
echo -e "${CYAN}${BOLD}║       HisnOS Secure Workstation — Recovery Mode          ║${RESET}"
echo -e "${CYAN}${BOLD}║                      v1.0                                ║${RESET}"
echo -e "${CYAN}${BOLD}╚══════════════════════════════════════════════════════════╝${RESET}"
echo ""
BOOT_SCORE="$(hisnos_flag get boot_score 2>/dev/null || echo unknown)"
SAFE_MODE="$(hisnos_flag isset boot_critical && echo "YES (critical)" || echo "NO")"
echo -e "  ${DIM}Kernel:      $(uname -r)${RESET}"
echo -e "  ${DIM}Boot score:  ${BOOT_SCORE}${RESET}"
echo -e "  ${DIM}Safe-mode:   ${SAFE_MODE}${RESET}"
echo -e "  ${DIM}Time (UTC):  $(date -u '+%Y-%m-%dT%H:%M:%SZ')${RESET}"
echo ""
echo -e "  ${YELLOW}${BOLD}WARNING: System has NOT fully booted. Changes affect the installed OS.${RESET}"
echo ""

# ─── Run fsck on root ────────────────────────────────────────────────────────
ROOT_DEV="$(hisnos_flag get root_device 2>/dev/null || findmnt -n -o SOURCE "$SYSROOT" 2>/dev/null | head -1)"
ROOT_FSTYPE="$(hisnos_flag get root_fstype 2>/dev/null || echo unknown)"
if [[ -n "$ROOT_DEV" ]] && [[ "$ROOT_DEV" != "unknown" ]]; then
    echo -e "${YELLOW}Running filesystem check on ${ROOT_DEV} (${ROOT_FSTYPE})...${RESET}"
    case "$ROOT_FSTYPE" in
        ext4|ext3|ext2)
            fsck -n "$ROOT_DEV" && echo -e "${GREEN}  Filesystem OK${RESET}" || \
                echo -e "${RED}  Errors found — use 'fsck -y ${ROOT_DEV}' in rescue shell${RESET}"
            ;;
        btrfs)
            echo -e "  ${DIM}btrfs: self-healing (check with: btrfs check ${ROOT_DEV})${RESET}"
            ;;
        xfs)
            echo -e "  ${DIM}xfs: check with: xfs_repair -n ${ROOT_DEV}${RESET}"
            ;;
        *)
            echo -e "  ${DIM}Filesystem type ${ROOT_FSTYPE}: no automatic check${RESET}"
            ;;
    esac
    echo ""
fi

# ─── Menu functions ───────────────────────────────────────────────────────────

fn_shell() {
    echo -e "\n${YELLOW}Rescue shell. Type 'exit' to return to menu.${RESET}\n"
    RECOVERY=1 bash --login 2>/dev/null || bash
}

fn_remount_rw() {
    echo -e "\n${YELLOW}Remounting root read-write...${RESET}"
    if mount -o remount,rw /; then
        echo -e "${GREEN}/ is now read-write${RESET}"
    else
        echo -e "${RED}Remount failed${RESET}"
    fi
    echo ""
}

fn_chroot() {
    if ! mountpoint -q "$SYSROOT" 2>/dev/null; then
        echo -e "${RED}sysroot ($SYSROOT) not mounted — cannot chroot${RESET}"
        return
    fi
    echo -e "\n${YELLOW}Chroot into installed system. Type 'exit' to return.${RESET}\n"
    # Bind necessary pseudo-filesystems
    for fs in proc sys dev run; do
        mount --bind "/$fs" "${SYSROOT}/$fs" 2>/dev/null || true
    done
    chroot "$SYSROOT" /bin/bash --login 2>/dev/null || \
        echo -e "${RED}chroot failed (try rescue shell)${RESET}"
    # Unmount on exit
    for fs in run dev sys proc; do
        umount "${SYSROOT}/$fs" 2>/dev/null || true
    done
    echo ""
}

fn_journal() {
    echo -e "\n${YELLOW}Boot journal (press q to exit):${RESET}\n"
    if command -v journalctl &>/dev/null; then
        journalctl -b --no-pager -n 100 2>/dev/null || \
            echo -e "${DIM}Journal not available (system not fully booted)${RESET}"
    else
        # Read the raw journal directory if journalctl unavailable
        echo -e "${DIM}journalctl unavailable — checking kernel ring buffer${RESET}"
        dmesg --level=err,warn 2>/dev/null | tail -50 || true
    fi
    echo ""
    read -rp "Press ENTER to continue..." _
}

fn_rollback() {
    echo -e "\n${YELLOW}OSTree rollback:${RESET}"
    if ! mountpoint -q "$SYSROOT" 2>/dev/null; then
        echo -e "${RED}sysroot not mounted${RESET}"
        return
    fi
    if command -v rpm-ostree &>/dev/null; then
        echo -e "Current deployments:"
        rpm-ostree status --sysroot="$SYSROOT" 2>/dev/null || \
            echo -e "${RED}rpm-ostree status failed${RESET}"
        echo ""
        read -rp "Rollback to previous deployment? (yes/no): " CONFIRM
        if [[ "$CONFIRM" == "yes" ]]; then
            rpm-ostree rollback --sysroot="$SYSROOT" 2>/dev/null && \
                echo -e "${GREEN}Rollback staged — reboot to apply${RESET}" || \
                echo -e "${RED}Rollback failed${RESET}"
        else
            echo "Rollback cancelled"
        fi
    elif command -v ostree &>/dev/null; then
        echo "Available refs:"
        ostree --repo="${SYSROOT}/ostree/repo" refs 2>/dev/null || true
        echo ""
        echo -e "${DIM}Run: ostree admin rollback --sysroot=${SYSROOT}${RESET}"
    else
        echo -e "${RED}rpm-ostree/ostree not available in initramfs${RESET}"
        echo "Rollback after booting: rpm-ostree rollback"
    fi
    echo ""
}

fn_firewall_reset() {
    echo -e "\n${YELLOW}Firewall reset:${RESET}"
    if ! command -v nft &>/dev/null; then
        echo -e "${RED}nft not available in initramfs${RESET}"
        echo "After boot: sudo systemctl restart nftables"
        return
    fi
    # Flush all tables
    nft flush ruleset 2>/dev/null && \
        echo -e "${GREEN}All nftables rules flushed${RESET}" || \
        echo -e "${RED}Flush failed${RESET}"
    # Try to restore from config
    NFT_CONF="${SYSROOT}/etc/nftables.conf"
    if [[ -f "$NFT_CONF" ]]; then
        read -rp "Restore nftables from ${NFT_CONF}? (yes/no): " CONFIRM
        [[ "$CONFIRM" == "yes" ]] && nft -f "$NFT_CONF" && \
            echo -e "${GREEN}Rules restored${RESET}" || \
            echo -e "${RED}Restore failed${RESET}"
    fi
    echo ""
}

fn_vault_check() {
    echo -e "\n${YELLOW}Vault integrity check:${RESET}"
    VAULT_DIR="${SYSROOT}/var/lib/hisnos"
    if [[ -d "$VAULT_DIR" ]]; then
        echo "State directory: OK ($VAULT_DIR)"
        ls -la "$VAULT_DIR" 2>/dev/null | head -20
    else
        echo -e "${RED}Vault state directory not found${RESET}"
    fi
    echo ""
    # Check for active gocryptfs mounts
    if findmnt | grep -q gocryptfs 2>/dev/null; then
        echo -e "${GREEN}gocryptfs: mounted${RESET}"
    else
        echo -e "${DIM}gocryptfs: not mounted (expected — system not fully booted)${RESET}"
    fi
    echo ""
}

fn_network() {
    echo -e "\n${YELLOW}Network status:${RESET}"
    ip addr show 2>/dev/null || echo -e "${DIM}ip unavailable${RESET}"
    echo ""
    ip route show 2>/dev/null | head -5
    echo ""
    if ping -c 2 -W 3 1.1.1.1 &>/dev/null 2>&1; then
        echo -e "${GREEN}External connectivity: OK${RESET}"
    else
        echo -e "${RED}External connectivity: FAILED${RESET}"
    fi
    echo ""
}

fn_ssh_rescue() {
    echo -e "\n${YELLOW}SSH rescue (dropbear):${RESET}"
    if ! command -v dropbear &>/dev/null; then
        echo -e "${RED}dropbear not in initramfs — rebuild with: dracut --add hisnos${RESET}"
        echo "  Install on live system: dnf install dropbear"
        return
    fi
    HOSTKEY="/tmp/hisnos-rescue-hostkey"
    [[ -f "$HOSTKEY" ]] || dropbearkey -t ed25519 -f "$HOSTKEY" &>/dev/null
    PORT=2222
    dropbear -s -F -E -p "$PORT" -r "$HOSTKEY" &
    DBPID=$!
    MY_IP="$(ip route get 1 2>/dev/null | awk '{print $NF; exit}')"
    echo -e "${GREEN}dropbear started (PID ${DBPID}) on port ${PORT}${RESET}"
    echo "  Connect: ssh root@${MY_IP:-<ip>} -p ${PORT}"
    echo -e "  ${RED}No password: add key to /root/.ssh/authorized_keys first${RESET}"
    echo ""
    read -rp "Press ENTER to return to menu (SSH stays running)..." _
}

fn_boot_score() {
    echo -e "\n${YELLOW}Boot health history:${RESET}"
    HEALTH_FILE="${SYSROOT}/var/lib/hisnos/boot-health.json"
    if [[ -f "$HEALTH_FILE" ]]; then
        echo "File: $HEALTH_FILE"
        cat "$HEALTH_FILE" 2>/dev/null
    else
        echo -e "${DIM}No boot health data available (first boot or state missing)${RESET}"
    fi
    echo ""
    read -rp "Press ENTER to continue..." _
}

# ─── Main menu ────────────────────────────────────────────────────────────────
print_menu() {
    echo ""
    echo -e "${CYAN}${BOLD}Recovery Menu${RESET}"
    echo -e "  ${BOLD}1${RESET}) Rescue shell"
    echo -e "  ${BOLD}2${RESET}) Chroot into installed system"
    echo -e "  ${BOLD}3${RESET}) Remount root read-write"
    echo -e "  ${BOLD}4${RESET}) OSTree rollback"
    echo -e "  ${BOLD}5${RESET}) Show boot journal"
    echo -e "  ${BOLD}6${RESET}) Network status"
    echo -e "  ${BOLD}7${RESET}) Firewall reset"
    echo -e "  ${BOLD}8${RESET}) Vault integrity check"
    echo -e "  ${BOLD}9${RESET}) Start SSH rescue (dropbear)"
    echo -e "  ${BOLD}0${RESET}) Boot health history"
    echo -e "  ${BOLD}r${RESET}) Reboot"
    echo -e "  ${BOLD}q${RESET}) Continue boot → rescue.target"
    echo ""
}

while true; do
    print_menu
    read -r -t 180 -p "Choice [q]: " CHOICE || { echo ""; CHOICE="q"; }
    CHOICE="${CHOICE:-q}"
    case "$CHOICE" in
        1) fn_shell ;;
        2) fn_chroot ;;
        3) fn_remount_rw ;;
        4) fn_rollback ;;
        5) fn_journal ;;
        6) fn_network ;;
        7) fn_firewall_reset ;;
        8) fn_vault_check ;;
        9) fn_ssh_rescue ;;
        0) fn_boot_score ;;
        r|R) reboot -f ;;
        q|Q) break ;;
        *) echo -e "${RED}Unknown: $CHOICE${RESET}" ;;
    esac
done

echo -e "\n${CYAN}[recovery] Continuing to systemd rescue.target...${RESET}\n"
exit 0
