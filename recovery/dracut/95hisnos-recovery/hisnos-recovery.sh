#!/usr/bin/env bash
# recovery/dracut/95hisnos-recovery/hisnos-recovery.sh
#
# Dracut pre-pivot hook activated by hisnos.recovery=1 on kernel cmdline.
#
# PRODUCTION UX (v1.0):
#   - Visual warning banner (ANSI colour, cannot be missed)
#   - fsck on root device before pivot
#   - Full interactive recovery menu:
#       1) Rescue shell
#       2) Remount root read-write
#       3) Journal tail
#       4) Network test (ping + ip addr)
#       5) Firewall reset (flush + restore hisnos_egress)
#       6) Vault status
#       7) Start SSH rescue listener (dropbear, optional)
#       8) Reboot
#       q) Continue boot → rescue.target
#   - Exits cleanly so systemd continues into rescue.target

# Source dracut helpers (getarg, etc.).
. /lib/dracut-lib.sh

# ── Guard: only activate in recovery mode ────────────────────────────────────
getarg "hisnos.recovery=1" > /dev/null 2>&1 || exit 0

# ── Colour codes ─────────────────────────────────────────────────────────────
RESET=$'\033[0m'
BOLD=$'\033[1m'
CYAN=$'\033[1;36m'
YELLOW=$'\033[1;33m'
RED=$'\033[1;31m'
GREEN=$'\033[1;32m'
DIM=$'\033[2m'

# ── Banner ───────────────────────────────────────────────────────────────────
clear 2>/dev/null || true
echo ""
echo "${CYAN}${BOLD}╔══════════════════════════════════════════════════════╗${RESET}"
echo "${CYAN}${BOLD}║        HisnOS Recovery Environment  v1.0             ║${RESET}"
echo "${CYAN}${BOLD}╚══════════════════════════════════════════════════════╝${RESET}"
echo ""
echo "  ${BOLD}WARNING:${RESET} You are in recovery mode. The system has NOT fully booted."
echo "  All changes made here affect the installed system."
echo ""
echo "  ${DIM}Kernel : $(uname -r)${RESET}"
echo "  ${DIM}Date   : $(date -u '+%Y-%m-%dT%H:%M:%SZ')${RESET}"

# ── Root device detection ─────────────────────────────────────────────────────
ROOT_DEV=$(findmnt -n -o SOURCE / 2>/dev/null || echo "unknown")
ROOT_FSTYPE=$(findmnt -n -o FSTYPE / 2>/dev/null || echo "unknown")
echo "  ${DIM}Root   : ${ROOT_DEV} (${ROOT_FSTYPE})${RESET}"
echo ""

# ── fsck ────────────────────────────────────────────────────────────────────
echo "${YELLOW}[recovery] Running read-only filesystem check on ${ROOT_DEV}…${RESET}"
if fsck -n "${ROOT_DEV}" > /tmp/fsck-out.txt 2>&1; then
  echo "${GREEN}[recovery] Filesystem OK${RESET}"
else
  echo "${RED}[recovery] Filesystem errors detected:${RESET}"
  cat /tmp/fsck-out.txt
  echo ""
  echo "${YELLOW}  Fix with: fsck -y ${ROOT_DEV}  (requires remount read-write first)${RESET}"
fi
echo ""

# ─────────────────────────────────────────────────────────────────────────────
# Menu functions
# ─────────────────────────────────────────────────────────────────────────────

do_shell() {
  echo "${YELLOW}Entering rescue shell. Type 'exit' to return to recovery menu.${RESET}"
  echo ""
  bash --login
}

do_remount_rw() {
  echo "${YELLOW}Remounting / read-write…${RESET}"
  if mount -o remount,rw /; then
    echo "${GREEN}Success. / is now read-write.${RESET}"
  else
    echo "${RED}Failed to remount read-write.${RESET}"
  fi
}

do_journal() {
  echo "${YELLOW}Last 80 journal lines (ctrl-C to stop):${RESET}"
  journalctl -n 80 --no-pager 2>/dev/null \
    || echo "${DIM}(journal not available — system not fully booted)${RESET}"
}

do_network_test() {
  echo "${YELLOW}Network status:${RESET}"
  echo ""
  echo "${BOLD}Interfaces:${RESET}"
  ip addr show 2>/dev/null || echo "  (ip command unavailable)"
  echo ""
  echo "${BOLD}Default routes:${RESET}"
  ip route show 2>/dev/null || echo "  (no routes)"
  echo ""
  echo "${BOLD}DNS servers:${RESET}"
  cat /etc/resolv.conf 2>/dev/null | grep nameserver || echo "  (none configured)"
  echo ""
  echo "${BOLD}Ping test (8.8.8.8):${RESET}"
  if ping -c 3 -W 3 8.8.8.8 &>/dev/null; then
    echo "${GREEN}  Connectivity: OK${RESET}"
  else
    echo "${RED}  Connectivity: FAILED (no external network)${RESET}"
    echo "  This is expected if no network interface is configured in recovery."
  fi
}

do_firewall_reset() {
  echo "${YELLOW}Firewall reset:${RESET}"
  if command -v nft &>/dev/null; then
    # Flush the gaming fast-path if present.
    nft flush table inet hisnos_gaming_fast 2>/dev/null && \
      nft delete table inet hisnos_gaming_fast 2>/dev/null && \
      echo "${GREEN}  hisnos_gaming_fast: flushed${RESET}" || true

    # Check base egress table.
    if nft list table inet hisnos_egress &>/dev/null; then
      echo "${GREEN}  hisnos_egress: present${RESET}"
    else
      echo "${RED}  hisnos_egress: MISSING${RESET}"
      echo "  Restore after boot: sudo systemctl restart nftables"
      # Attempt to load the base config if available.
      NFT_CONF="/etc/nftables.conf"
      if [[ -f "${NFT_CONF}" ]]; then
        echo "${YELLOW}  Attempting to load ${NFT_CONF}…${RESET}"
        nft -f "${NFT_CONF}" 2>/dev/null && \
          echo "${GREEN}  Loaded ${NFT_CONF}${RESET}" || \
          echo "${RED}  Failed to load ${NFT_CONF}${RESET}"
      fi
    fi
  else
    echo "${RED}  nft command not available in initramfs.${RESET}"
  fi
}

do_vault_status() {
  echo "${YELLOW}Vault status:${RESET}"
  STATE_DIR="/var/lib/hisnos"
  STATE_FILE="${STATE_DIR}/onboarding-state.json"

  if mountpoint -q /var/lib/hisnos 2>/dev/null || [[ -d "${STATE_DIR}" ]]; then
    if [[ -f "${STATE_FILE}" ]]; then
      echo "${GREEN}  State file: ${STATE_FILE}${RESET}"
      cat "${STATE_FILE}" 2>/dev/null | head -30
    else
      echo "${DIM}  State file not present (onboarding not yet run)${RESET}"
    fi
  else
    echo "${RED}  /var/lib/hisnos not mounted. Mount root read-write (option 2) first.${RESET}"
  fi

  # Check for mounted vault.
  if findmnt | grep -q gocryptfs 2>/dev/null; then
    echo "${GREEN}  gocryptfs vault: mounted${RESET}"
  else
    echo "${DIM}  gocryptfs vault: not mounted${RESET}"
  fi
}

do_ssh_rescue() {
  echo "${YELLOW}SSH rescue:${RESET}"
  if ! command -v dropbear &>/dev/null; then
    echo "${RED}  dropbear not available in initramfs.${RESET}"
    echo "  Add it: include dropbear in the dracut module (module-setup.sh inst_multiple dropbear)"
    return
  fi

  # Generate a temporary host key if absent.
  HOSTKEY="/tmp/dropbear_host_key"
  if [[ ! -f "${HOSTKEY}" ]]; then
    dropbearkey -t rsa -f "${HOSTKEY}" &>/dev/null
  fi

  # Find a free port.
  PORT=2222

  dropbear -s -F -E -p "${PORT}" -r "${HOSTKEY}" -P /tmp/dropbear.pid &>/dev/null &
  DROPBEAR_PID=$!

  # Detect current IP.
  MY_IP=$(ip route get 1 2>/dev/null | awk '{print $NF; exit}' || echo "unknown")

  echo "${GREEN}  dropbear SSH started on port ${PORT} (PID ${DROPBEAR_PID})${RESET}"
  echo "  Connect: ssh root@${MY_IP} -p ${PORT}"
  echo "  ${RED}WARNING: no password auth — add your public key to /root/.ssh/authorized_keys${RESET}"
  echo ""
  echo "  Press ENTER to return to the menu (dropbear keeps running)."
  read -r _
}

# ─────────────────────────────────────────────────────────────────────────────
# Main menu loop
# ─────────────────────────────────────────────────────────────────────────────

print_menu() {
  echo ""
  echo "${CYAN}${BOLD}Recovery Menu${RESET}"
  echo "  ${BOLD}1${RESET}) Rescue shell"
  echo "  ${BOLD}2${RESET}) Remount root read-write"
  echo "  ${BOLD}3${RESET}) Show journal"
  echo "  ${BOLD}4${RESET}) Network test"
  echo "  ${BOLD}5${RESET}) Firewall reset"
  echo "  ${BOLD}6${RESET}) Vault status"
  echo "  ${BOLD}7${RESET}) Start SSH rescue listener"
  echo "  ${BOLD}8${RESET}) Reboot"
  echo "  ${BOLD}q${RESET}) Continue boot (proceed to rescue.target)"
  echo ""
}

while true; do
  print_menu
  read -r -t 120 -p "Choice [q]: " CHOICE || { echo ""; CHOICE="q"; }
  CHOICE="${CHOICE:-q}"

  case "${CHOICE}" in
    1) do_shell ;;
    2) do_remount_rw ;;
    3) do_journal ;;
    4) do_network_test ;;
    5) do_firewall_reset ;;
    6) do_vault_status ;;
    7) do_ssh_rescue ;;
    8) reboot -f ;;
    q|Q) break ;;
    *) echo "${RED}Unknown option: ${CHOICE}${RESET}" ;;
  esac
done

echo ""
echo "${CYAN}[recovery] Exiting recovery menu — continuing to systemd rescue.target…${RESET}"
echo ""
exit 0
