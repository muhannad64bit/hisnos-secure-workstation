#!/usr/bin/env bash
# hisnos-egress.sh — HisnOS firewall mode control
#
# Manages the HisnOS nftables ruleset lifecycle:
#   observe  — staging mode (ACCEPT policy + logging what would be denied)
#   enforce  — enforcement mode (DROP policy, fail-closed NFQUEUE)
#   status   — show current mode, active rules summary
#   flush    — emergency: remove all HisnOS rules immediately
#   reload   — reload current mode from /etc/nftables/ (after config changes)
#   analyse  — parse observe-mode logs to show top would-block flows
#
# Dead-man timer (enforce only):
#   When switching to enforcement, a systemd timer automatically reverts to
#   observe mode after REVERT_TIMEOUT seconds. Cancel it once you confirm
#   networking works. This prevents getting locked out.
#
# Usage:
#   sudo ./hisnos-egress.sh observe
#   sudo ./hisnos-egress.sh enforce [--timeout 300]  # default: 300s revert timer
#   sudo ./hisnos-egress.sh enforce --no-timer        # skip revert timer (advanced)
#   sudo ./hisnos-egress.sh status
#   sudo ./hisnos-egress.sh flush                     # emergency recovery
#   sudo ./hisnos-egress.sh reload
#   sudo ./hisnos-egress.sh analyse [--lines 50]

set -euo pipefail

# ── Config ─────────────────────────────────────────────────────────────────
NFT_DIR="/etc/nftables"
HISNOS_BASE="${NFT_DIR}/hisnos-base.nft"
HISNOS_UPDATES="${NFT_DIR}/hisnos-updates.nft"
HISNOS_OBSERVE="${NFT_DIR}/hisnos-observe.nft"
MODE_FILE="/run/hisnos-egress-mode"       # ephemeral: cleared on reboot
TIMER_UNIT="hisnos-egress-revert.timer"  # transient systemd timer
REVERT_TIMEOUT=300                        # seconds before auto-revert

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BOLD='\033[1m'; NC='\033[0m'
info()    { echo -e "${GREEN}[egress]${NC} $*"; }
warn()    { echo -e "${YELLOW}[egress WARN]${NC} $*"; }
error()   { echo -e "${RED}[egress ERROR]${NC} $*" >&2; exit 1; }
section() { echo -e "\n${BOLD}── $* ──${NC}"; }

[[ "${EUID}" -eq 0 ]] || error "Must run as root (use sudo)"

# ── Helpers ────────────────────────────────────────────────────────────────
nft_check() {
    # Dry-run validate a ruleset file before applying
    local f="$1"
    nft -c -f "${f}" 2>/dev/null || {
        error "Syntax error in ${f} — run: sudo nft -c -f ${f}"
    }
}

current_mode() {
    cat "${MODE_FILE}" 2>/dev/null || echo "none"
}

table_exists() {
    nft list table inet hisnos &>/dev/null
}

cancel_revert_timer() {
    if systemctl is-active --quiet "${TIMER_UNIT}" 2>/dev/null; then
        systemctl stop "${TIMER_UNIT}" 2>/dev/null || true
        info "Dead-man timer cancelled."
    fi
}

# ── Command dispatch ───────────────────────────────────────────────────────
CMD="${1:-status}"
shift || true

case "${CMD}" in

# ──────────────────────────────────────────────────────────────────────────
observe)
    section "Loading OBSERVE mode"
    [[ -f "${HISNOS_OBSERVE}" ]] || error "${HISNOS_OBSERVE} not found. Run bootstrap/post-install.sh first."

    nft_check "${HISNOS_OBSERVE}"
    info "Syntax OK."

    # Cancel any active revert timer (entering observe is already safe)
    cancel_revert_timer

    nft -f "${HISNOS_OBSERVE}"
    # Populate allowlist sets so observe-mode logging shows accurate ALLOW hits
    [[ -f "${HISNOS_UPDATES}" ]] && nft -f "${HISNOS_UPDATES}" && info "Allowlist sets populated."

    echo "observe" > "${MODE_FILE}"
    info "OBSERVE mode active."
    echo ""
    echo "  Traffic flows freely. Would-deny flows are logged."
    echo ""
    echo "  Watch live:   journalctl -k -f -g 'HISNOS-OBS'"
    echo "  Analyse:      sudo ${0} analyse"
    echo "  Switch to enforce when ready:   sudo ${0} enforce"
    ;;

# ──────────────────────────────────────────────────────────────────────────
enforce)
    # Parse flags
    USE_TIMER=true
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --timeout)   REVERT_TIMEOUT="${2:-300}"; shift 2 ;;
            --timeout=*) REVERT_TIMEOUT="${1#--timeout=}"; shift ;;
            --no-timer)  USE_TIMER=false; shift ;;
            *) shift ;;
        esac
    done

    section "Loading ENFORCE mode"
    [[ -f "${HISNOS_BASE}" ]] || error "${HISNOS_BASE} not found."
    [[ -f "${HISNOS_UPDATES}" ]] || error "${HISNOS_UPDATES} not found."

    # Syntax validation before touching live rules
    info "Validating ruleset syntax..."
    nft_check "${HISNOS_BASE}"
    nft_check "${HISNOS_UPDATES}"
    info "Syntax OK."

    # ── Dead-man timer ────────────────────────────────────────────────────
    # A transient systemd timer fires after REVERT_TIMEOUT seconds and
    # reloads observe mode. If your terminal is gone due to a firewall
    # lockout, this restores access automatically.
    #
    # Cancel after confirming connectivity: sudo systemctl stop hisnos-egress-revert.timer
    # Or: sudo ${0} status (offers cancel if timer is running)
    if ${USE_TIMER}; then
        warn "Dead-man timer: observe mode will be restored in ${REVERT_TIMEOUT}s if not cancelled."
        warn "After confirming network works: sudo ${0} status  (then press y to cancel timer)"
        echo ""

        # systemd-run creates a transient one-shot timer that calls this script
        systemd-run \
            --unit="${TIMER_UNIT}" \
            --description="HisnOS egress auto-revert to observe" \
            --on-active="${REVERT_TIMEOUT}" \
            --no-block \
            bash -c "nft -f ${HISNOS_OBSERVE} && nft -f ${HISNOS_UPDATES} && echo 'observe' > ${MODE_FILE} && logger -t hisnos-egress 'Auto-reverted to observe mode (dead-man timer)'"

        info "Timer started (unit: ${TIMER_UNIT}). Fires in ${REVERT_TIMEOUT}s."
    else
        warn "--no-timer: enforcement is permanent until manually changed."
        warn "If you lose network access, reboot and select previous ostree deployment."
    fi

    # Load enforcement
    nft -f "${HISNOS_BASE}"
    nft -f "${HISNOS_UPDATES}"

    echo "enforce" > "${MODE_FILE}"
    info "ENFORCE mode active."
    echo ""
    echo "  Default-deny outbound is now active."
    echo "  DNS (127.0.0.53), NTP, Fedora/Flatpak updates allowed."
    echo "  All other traffic → OpenSnitch NFQUEUE (fail-closed)."
    echo ""
    echo "  Test connectivity: sudo tests/test-egress.sh --verify"

    if ${USE_TIMER}; then
        echo ""
        echo "  Cancel timer after confirming network works:"
        echo "    sudo systemctl stop ${TIMER_UNIT}"
        echo "    sudo ${0} status"
    fi
    ;;

# ──────────────────────────────────────────────────────────────────────────
flush)
    section "EMERGENCY FLUSH — removing all HisnOS firewall rules"
    warn "This removes all nftables rules. Network traffic is unrestricted."
    echo ""
    read -r -p "  Confirm emergency flush? [y/N] " CONFIRM
    [[ "${CONFIRM}" =~ ^[Yy]$ ]] || { info "Aborted."; exit 0; }

    cancel_revert_timer

    if table_exists; then
        nft delete table inet hisnos
        info "Table 'inet hisnos' deleted."
    else
        info "Table 'inet hisnos' was not loaded."
    fi

    echo "none" > "${MODE_FILE}"
    info "Flush complete. Network is fully open."
    echo ""
    echo "  To restore observe mode:   sudo ${0} observe"
    echo "  To restore enforcement:    sudo ${0} enforce"
    ;;

# ──────────────────────────────────────────────────────────────────────────
reload)
    MODE="$(current_mode)"
    section "Reloading ruleset (current mode: ${MODE})"

    case "${MODE}" in
        enforce)
            nft_check "${HISNOS_BASE}" && nft_check "${HISNOS_UPDATES}"
            nft -f "${HISNOS_BASE}"
            nft -f "${HISNOS_UPDATES}"
            info "Enforcement ruleset reloaded."
            ;;
        observe)
            nft_check "${HISNOS_OBSERVE}"
            nft -f "${HISNOS_OBSERVE}"
            [[ -f "${HISNOS_UPDATES}" ]] && nft -f "${HISNOS_UPDATES}"
            info "Observe ruleset reloaded."
            ;;
        none|*)
            error "No active mode. Load a mode first: sudo ${0} observe"
            ;;
    esac
    ;;

# ──────────────────────────────────────────────────────────────────────────
status)
    section "HisnOS Egress Status"
    MODE="$(current_mode)"
    echo "  Mode file:    ${MODE_FILE}"
    echo "  Current mode: ${BOLD}${MODE}${NC}"
    echo ""

    if table_exists; then
        echo "  nftables table: inet hisnos (loaded)"
        # Show chain policies
        nft list table inet hisnos 2>/dev/null \
            | grep -E "chain (input|output|forward)" \
            | sed 's/^/  /'
    else
        echo "  nftables table: NOT loaded"
    fi

    echo ""
    # Dead-man timer status
    if systemctl is-active --quiet "${TIMER_UNIT}" 2>/dev/null; then
        REMAINING=$(systemctl show "${TIMER_UNIT}" --property=NextElapseUSecMonotonic \
            2>/dev/null | cut -d= -f2 || echo "unknown")
        warn "Dead-man revert timer is ACTIVE (unit: ${TIMER_UNIT})"
        echo ""
        read -r -p "  Cancel revert timer now? [y/N] " CONFIRM
        [[ "${CONFIRM}" =~ ^[Yy]$ ]] && cancel_revert_timer
    else
        info "Dead-man timer: not active"
    fi

    echo ""
    echo "  Quick log check:"
    journalctl -k --no-pager -n 5 -g "HISNOS-" 2>/dev/null \
        | sed 's/^/    /' || echo "    (no recent HisnOS log entries)"
    ;;

# ──────────────────────────────────────────────────────────────────────────
analyse)
    # Parse observe-mode logs to surface most-blocked flows
    LINES=50
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --lines)   LINES="${2:-50}"; shift 2 ;;
            --lines=*) LINES="${1#--lines=}"; shift ;;
            *) shift ;;
        esac
    done

    section "Observe mode log analysis (top ${LINES} flows)"
    echo ""
    echo "  ── WOULD-QUEUE (needs OpenSnitch rule or allowlist entry) ──"
    journalctl -k --no-pager -g "HISNOS-OBS-WOULD-QUEUE" 2>/dev/null \
        | grep -oE "DST=[0-9.]+ " \
        | sort | uniq -c | sort -rn \
        | head "${LINES}" \
        | sed 's/^/    /' \
        || echo "    (no entries — observe mode not loaded or no log data)"

    echo ""
    echo "  ── WOULD-DROP-IN (unexpected inbound) ──"
    journalctl -k --no-pager -g "HISNOS-OBS-WOULD-DROP-IN" 2>/dev/null \
        | grep -oE "SRC=[0-9.]+ " \
        | sort | uniq -c | sort -rn \
        | head 20 \
        | sed 's/^/    /' \
        || echo "    (none)"

    echo ""
    echo "  ── ALLOWED (passing kernel allowlists) ──"
    journalctl -k --no-pager -g "HISNOS-OBS-ALLOW" 2>/dev/null \
        | grep -oE "DST=[0-9.]+ DPT=[0-9]+" \
        | sort | uniq -c | sort -rn \
        | head 20 \
        | sed 's/^/    /' \
        || echo "    (none — populate allowlist sets first)"

    echo ""
    echo "  Full log: journalctl -k -g 'HISNOS-OBS' --no-pager | less"
    ;;

# ──────────────────────────────────────────────────────────────────────────
*)
    echo "Usage: sudo ${0} <command>"
    echo ""
    echo "Commands:"
    echo "  observe              Load staging mode (ACCEPT + log)"
    echo "  enforce [--timeout N] [--no-timer]   Load enforcement (DROP + NFQUEUE)"
    echo "  flush                Emergency: remove all HisnOS rules"
    echo "  reload               Reload current mode from /etc/nftables/"
    echo "  status               Show current mode and recent log entries"
    echo "  analyse [--lines N]  Parse observe logs for top blocked flows"
    ;;

esac
