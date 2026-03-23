#!/bin/bash
# /bin/hisnos-emergency — HisnOS Live Boot Emergency Recovery UI
#
# Launched by hisnos_die() when a fatal error occurs during live boot.
# Provides:
#   - Clear error display with context
#   - Boot log viewer
#   - Manual mount retry
#   - Minimal shell escape
#   - Reboot / power-off
#
# This script runs inside the initramfs, before switch_root.
# Only standard POSIX utilities + what module-setup.sh installs are available.

REASON="${*:-Unknown fatal error during HisnOS live boot.}"
LOG=/run/hisnos/live-boot.log
FAIL=/run/hisnos/fail-reason
HISNOS_RUN=/run/hisnos

# ANSI colour helpers (safe — we check tty).
_RED='\033[0;31m'; _GRN='\033[0;32m'; _YLW='\033[0;33m'
_CYN='\033[0;36m'; _WHT='\033[1;37m'; _RST='\033[0m'
_BLD='\033[1m';    _DIM='\033[2m'
if ! [ -t 1 ]; then
    _RED=''; _GRN=''; _YLW=''; _CYN=''; _WHT=''; _RST=''; _BLD=''; _DIM=''
fi

# ── Display helpers ────────────────────────────────────────────────────────
banner() {
    clear 2>/dev/null || printf '\033[H\033[2J'
    printf '\n'
    printf "${_RED}${_BLD}%s${_RST}\n" \
        "╔══════════════════════════════════════════════════════════╗"
    printf "${_RED}${_BLD}%s${_RST}\n" \
        "║         HisnOS LIVE BOOT — RECOVERY CONSOLE             ║"
    printf "${_RED}${_BLD}%s${_RST}\n" \
        "╚══════════════════════════════════════════════════════════╝"
    printf '\n'
}

show_error() {
    printf "${_RED}${_BLD}FATAL ERROR:${_RST}\n\n"
    printf "${_YLW}%s${_RST}\n\n" "${REASON}"
    if [[ -f "${FAIL}" ]]; then
        printf "${_DIM}(Details: %s)${_RST}\n\n" "$(<"${FAIL}")"
    fi
}

show_menu() {
    printf "${_WHT}${_BLD}Recovery options:${_RST}\n\n"
    printf "  ${_CYN}[1]${_RST} View boot log\n"
    printf "  ${_CYN}[2]${_RST} Retry live root mount\n"
    printf "  ${_CYN}[3]${_RST} Open minimal rescue shell\n"
    printf "  ${_CYN}[4]${_RST} Show block devices\n"
    printf "  ${_CYN}[5]${_RST} Show kernel messages (dmesg)\n"
    printf "  ${_CYN}[6]${_RST} Reboot\n"
    printf "  ${_CYN}[7]${_RST} Power off\n"
    printf '\n'
    printf "${_WHT}Enter choice [1-7]: ${_RST}"
}

view_log() {
    printf "\n${_BLD}=== Boot Log: %s ===${_RST}\n" "${LOG}"
    if [[ -f "${LOG}" ]]; then
        cat "${LOG}"
    elif [[ -f /tmp/hisnos-live-boot.log ]]; then
        cat /tmp/hisnos-live-boot.log
    else
        printf "(Log not found)\n"
    fi
    printf "\n${_DIM}[Press Enter to return to menu]${_RST}"
    read -r _
}

retry_mount() {
    printf "\n${_YLW}Retrying live root mount...${_RST}\n\n"
    # Source the library and retry the mount hook.
    if [[ -f /lib/dracut/hisnos-live-lib.sh ]]; then
        . /lib/dracut/hisnos-live-lib.sh
        # Clear cached source-dev to force re-detection.
        rm -f "${HISNOS_RUN}/source-dev" 2>/dev/null
        udevadm settle --timeout=10 2>/dev/null || true
        if hisnos_find_source_dev; then
            echo "${HISNOS_SOURCE_DEV}" > "${HISNOS_RUN}/source-dev"
            printf "${_GRN}Source device found: %s${_RST}\n" "${HISNOS_SOURCE_DEV}"
            printf "${_YLW}Running mount script...${_RST}\n"
            if bash /lib/dracut/hooks/mount/30-hisnos-live-root.sh 2>&1; then
                printf "\n${_GRN}${_BLD}Mount succeeded! Attempting to continue boot...${_RST}\n"
                sleep 2
                # Signal dracut to continue.
                echo "" > /run/initramfs/root-mounted 2>/dev/null || true
                exit 0
            else
                printf "\n${_RED}Mount still failing. Check log for details.${_RST}\n"
            fi
        else
            printf "${_RED}Still cannot find source device.${_RST}\n"
            printf "Connected block devices:\n"
            lsblk 2>/dev/null || ls /dev/sd* /dev/sr* /dev/vd* 2>/dev/null || true
        fi
    else
        printf "${_RED}Live library not found — cannot retry.${_RST}\n"
    fi
    printf "\n${_DIM}[Press Enter to return to menu]${_RST}"
    read -r _
}

open_shell() {
    printf "\n${_YLW}${_BLD}Opening rescue shell.${_RST}\n"
    printf "${_DIM}Type 'exit' to return to the recovery menu.${_RST}\n"
    printf "${_DIM}Useful commands: lsblk, blkid, dmesg, mount, losetup -l${_RST}\n\n"
    export PS1='[hisnos-rescue \w]# '
    bash --login 2>&1 || sh
}

show_blkdevs() {
    printf "\n${_BLD}=== Block Devices ===${_RST}\n"
    lsblk -o NAME,SIZE,TYPE,FSTYPE,LABEL,MOUNTPOINT 2>/dev/null || \
        ls -la /dev/sd* /dev/sr* /dev/vd* /dev/loop* 2>/dev/null || \
        printf "(lsblk not available)\n"
    printf "\n${_BLD}=== blkid ===${_RST}\n"
    blkid 2>/dev/null || printf "(blkid not available)\n"
    printf "\n${_DIM}[Press Enter to return to menu]${_RST}"
    read -r _
}

show_dmesg() {
    printf "\n${_BLD}=== Kernel Messages (last 50 lines) ===${_RST}\n"
    dmesg 2>/dev/null | tail -50 || cat /proc/kmsg 2>/dev/null | head -50 || true
    printf "\n${_DIM}[Press Enter to return to menu]${_RST}"
    read -r _
}

do_reboot() {
    printf "\n${_YLW}Rebooting in 3 seconds...${_RST}\n"
    sleep 3
    reboot -f 2>/dev/null || echo b > /proc/sysrq-trigger
}

do_poweroff() {
    printf "\n${_YLW}Powering off in 3 seconds...${_RST}\n"
    sleep 3
    poweroff -f 2>/dev/null || echo o > /proc/sysrq-trigger
}

# ── Main loop ──────────────────────────────────────────────────────────────
main() {
    # Ensure we're on a usable terminal.
    stty sane 2>/dev/null || true
    reset 2>/dev/null || clear 2>/dev/null || true

    while true; do
        banner
        show_error
        show_menu

        local choice
        read -r choice

        case "${choice}" in
            1) view_log ;;
            2) retry_mount ;;
            3) open_shell ;;
            4) show_blkdevs ;;
            5) show_dmesg ;;
            6) do_reboot ;;
            7) do_poweroff ;;
            *) printf "${_RED}Invalid choice: %s${_RST}\n" "${choice}"; sleep 1 ;;
        esac
    done
}

main
