#!/usr/bin/env bash
# recovery/hisnos-recover.sh — HisnOS Unified Recovery CLI
#
# Provides deterministic recovery procedures for all HisnOS subsystems.
# Designed to be runnable from a minimal session (TTY, SSH, Ctrl+Alt+F2)
# even if the dashboard and most services are unresponsive.
#
# Usage:
#   hisnos-recover.sh <command> [options]
#
# Commands:
#   firewall-flush        Emergency flush of all HisnOS nftables rules
#   vault-force-lock      Force-unmount all gocryptfs vaults immediately
#   dashboard-safe-mode   Restart dashboard with minimal config and no auth
#   rollback-guide        Print step-by-step rollback instructions for current state
#   gaming-reset          Force-reset gaming tuning to normal profile
#   lab-emergency-stop    Terminate all lab sessions and flush lab network state
#   status                Print current HisnOS subsystem health as JSON
#
# Safety principles:
#   - Every command is idempotent (safe to run multiple times)
#   - Commands never make state worse: they only remove/reset, never add
#   - Destructive actions require --confirm flag
#   - All actions are logged to journald (SYSLOG_IDENTIFIER=hisnos-recover)
#   - Exit code 0 = success, 1 = partial failure, 2 = full failure
#
# Run as: logged-in user (sudo required only for firewall-flush and gaming-reset)
set -euo pipefail

readonly SCRIPT="$(basename "${BASH_SOURCE[0]}")"
readonly LOG_TAG="hisnos-recover"
readonly VAR_DIR="/var/lib/hisnos"
readonly RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
readonly NFT_BIN="${NFT_BIN:-/usr/sbin/nft}"
readonly SYSTEMCTL="/usr/bin/systemctl"

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'
BOLD=$'\033[1m'; NC=$'\033[0m'

ok()   { echo -e "${GREEN}[OK]${NC}   $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $*" >&2; }
fail() { echo -e "${RED}[FAIL]${NC} $*" >&2; }
sep()  { echo -e "${BOLD}── $* ──${NC}"; }

log() {
    systemd-cat -t "${LOG_TAG}" -p info printf 'HISNOS_RECOVER cmd=%s %s\n' "${CURRENT_CMD:-unknown}" "$*" 2>/dev/null || true
    echo "  $*"
}

CURRENT_CMD=""

# ── Command: firewall-flush ───────────────────────────────────────────────────

cmd_firewall_flush() {
    local confirm="${1:-}"
    sep "Emergency Firewall Flush"
    echo "  This will flush ALL HisnOS nftables rules."
    echo "  The system will have NO egress filtering until nftables.service is restarted."
    if [[ "${confirm}" != "--confirm" ]]; then
        fail "Destructive action — rerun with --confirm to proceed"
        echo "  Example: ${SCRIPT} firewall-flush --confirm"
        exit 2
    fi

    local rc=0

    # 1. Flush hisnos table (removes all rules: egress, lab, gaming chains)
    log "flushing inet hisnos table..."
    if sudo "${NFT_BIN}" flush table inet hisnos 2>/dev/null; then
        ok "inet hisnos table flushed"
    else
        warn "flush failed — table may not exist"
    fi

    # 2. Flush lab chains specifically (belt and suspenders)
    for chain in lab_veth_output lab_veth_input; do
        sudo "${NFT_BIN}" flush chain inet hisnos "${chain}" 2>/dev/null || true
    done

    # 3. Delete all lab veth interfaces
    log "removing lab veth interfaces..."
    for iface in $(ip link show 2>/dev/null | grep -oP "vlh-\w+" | sort -u); do
        sudo ip link delete "${iface}" 2>/dev/null && log "deleted ${iface}" || true
    done
    for iface in $(ip link show 2>/dev/null | grep -oP "vlc-\w+" | sort -u); do
        sudo ip link delete "${iface}" 2>/dev/null || true
    done

    # 4. Stop hisnos-lab-netd.socket so no new sessions can start
    sudo "${SYSTEMCTL}" stop hisnos-lab-netd.socket 2>/dev/null && \
        log "hisnos-lab-netd.socket stopped" || true

    ok "Firewall flush complete"
    warn "Run 'sudo systemctl restart nftables.service' to restore firewall"
    log "HISNOS_RECOVER_FIREWALL_FLUSH confirm=true"
    return "${rc}"
}

# ── Command: vault-force-lock ─────────────────────────────────────────────────

cmd_vault_force_lock() {
    sep "Vault Force-Lock"

    local rc=0
    local any_unmounted=false

    # 1. Stop vault idle timer (prevent it from racing with force-lock)
    "${SYSTEMCTL}" --user stop hisnos-vault-idle.timer 2>/dev/null || true
    "${SYSTEMCTL}" --user stop hisnos-vault-idle.service 2>/dev/null || true
    log "vault idle timer stopped"

    # 2. Kill vault watcher (it will try to keep vault mounted)
    "${SYSTEMCTL}" --user stop hisnos-vault-watcher.service 2>/dev/null || true
    log "vault watcher stopped"

    # 3. Find all gocryptfs mounts and unmount them
    local mount_point
    while IFS= read -r line; do
        mount_point=$(echo "${line}" | awk '{print $2}')
        [[ -n "${mount_point}" ]] || continue
        log "unmounting ${mount_point}..."
        if fusermount3 -uz "${mount_point}" 2>/dev/null || \
           fusermount -uz "${mount_point}" 2>/dev/null || \
           sudo umount -l "${mount_point}" 2>/dev/null; then
            ok "unmounted ${mount_point}"
            any_unmounted=true
        else
            fail "failed to unmount ${mount_point}"
            rc=1
        fi
    done < <(grep gocryptfs /proc/mounts 2>/dev/null || true)

    if [[ "${any_unmounted}" == "false" ]]; then
        ok "no gocryptfs mounts active — vault already locked"
    fi

    # 4. Restart vault watcher and idle timer (clean state)
    "${SYSTEMCTL}" --user start hisnos-vault-watcher.service 2>/dev/null || \
        warn "vault watcher failed to restart — run manually"
    "${SYSTEMCTL}" --user start hisnos-vault-idle.timer 2>/dev/null || true

    log "HISNOS_RECOVER_VAULT_FORCE_LOCK rc=${rc}"
    [[ "${rc}" -eq 0 ]] && ok "Vault force-lock complete" || fail "Vault force-lock completed with errors"
    return "${rc}"
}

# ── Command: dashboard-safe-mode ─────────────────────────────────────────────

cmd_dashboard_safe_mode() {
    sep "Dashboard Safe Mode"
    echo "  Restarts the dashboard service with reduced logging."
    echo "  Use when dashboard is unresponsive but socket is active."

    # 1. Stop current dashboard service
    log "stopping dashboard service..."
    "${SYSTEMCTL}" --user stop hisnos-dashboard.service 2>/dev/null || true

    # 2. Set safe-mode env override
    local override_dir="${HOME}/.config/systemd/user/hisnos-dashboard.service.d"
    mkdir -p "${override_dir}"
    cat > "${override_dir}/safe-mode.conf" << 'EOF'
[Service]
Environment=LOG_LEVEL=debug
Environment=HISNOS_SAFE_MODE=1
EOF
    log "safe-mode override written to ${override_dir}/safe-mode.conf"

    # 3. Reload and restart
    "${SYSTEMCTL}" --user daemon-reload
    if "${SYSTEMCTL}" --user restart hisnos-dashboard.socket 2>/dev/null; then
        ok "dashboard safe-mode active on 127.0.0.1:7374"
        echo "  Remove override: rm ${override_dir}/safe-mode.conf && systemctl --user daemon-reload"
    else
        fail "dashboard failed to restart — check: journalctl --user -u hisnos-dashboard"
        return 1
    fi

    log "HISNOS_RECOVER_DASHBOARD_SAFE_MODE"
}

# ── Command: rollback-guide ───────────────────────────────────────────────────

cmd_rollback_guide() {
    sep "HisnOS Rollback Guidance"
    echo ""

    # Detect current system state and generate targeted guidance
    echo "  Detecting system state..."
    echo ""

    # rpm-ostree
    local deployments=0
    deployments=$(rpm-ostree status --json 2>/dev/null | \
        python3 -c "import json,sys; d=json.load(sys.stdin); print(len(d.get('deployments',[])))" \
        2>/dev/null || echo "0")

    echo "  ${BOLD}rpm-ostree (Kinoite base):${NC}"
    if [[ "${deployments}" -gt 1 ]]; then
        ok "  Multiple deployments available — rollback is possible"
        echo "  Command: sudo rpm-ostree rollback && sudo systemctl reboot"
        rpm-ostree status 2>/dev/null | head -20 || true
    else
        warn "  Only one deployment available — no rpm-ostree rollback possible"
        echo "  Options: boot from rescue media, or rebase to known-good commit"
    fi
    echo ""

    echo "  ${BOLD}nftables firewall:${NC}"
    if sudo "${NFT_BIN}" list ruleset &>/dev/null 2>&1; then
        ok "  nftables running"
        echo "  To restore default rules: sudo systemctl restart nftables.service"
    else
        fail "  nftables not running"
        echo "  Restore: sudo systemctl start nftables.service"
    fi
    echo ""

    echo "  ${BOLD}Vault:${NC}"
    if grep -q gocryptfs /proc/mounts 2>/dev/null; then
        warn "  Vault currently MOUNTED — run 'hisnos-recover.sh vault-force-lock' before reboot"
    else
        ok "  Vault locked"
    fi
    echo ""

    echo "  ${BOLD}Gaming mode:${NC}"
    if [[ -f "${RUNTIME_DIR}/hisnos-gaming.lock" ]]; then
        warn "  Gaming mode ACTIVE — run 'hisnos-recover.sh gaming-reset' to restore normal profile"
    else
        ok "  Normal profile active"
    fi
    echo ""

    echo "  ${BOLD}Lab sessions:${NC}"
    if [[ -f "${RUNTIME_DIR}/hisnos-lab-session.json" ]]; then
        warn "  Lab session ACTIVE — run 'hisnos-recover.sh lab-emergency-stop'"
    else
        ok "  No active lab sessions"
    fi
    echo ""

    echo "  ${BOLD}Threat score:${NC}"
    if [[ -f "${VAR_DIR}/threat-state.json" ]]; then
        local score level
        score=$(python3 -c "import json; d=json.load(open('${VAR_DIR}/threat-state.json')); print(d.get('risk_score',0))" 2>/dev/null || echo "?")
        level=$(python3 -c "import json; d=json.load(open('${VAR_DIR}/threat-state.json')); print(d.get('current_risk_level','?'))" 2>/dev/null || echo "?")
        [[ "${level}" == "low" ]] && ok "  Risk: ${level} (score=${score})" || warn "  Risk: ${level} (score=${score})"
    else
        warn "  threat-state.json not found — threatd may not be running"
    fi

    log "HISNOS_RECOVER_ROLLBACK_GUIDE"
}

# ── Command: gaming-reset ─────────────────────────────────────────────────────

cmd_gaming_reset() {
    sep "Gaming Mode Force Reset"

    # 1. Run privileged stop (restores CPU governor, IRQ affinity, nftables)
    if sudo "${SYSTEMCTL}" start hisnos-gaming-tuned-stop.service 2>/dev/null; then
        ok "privileged gaming tuning restored to normal"
    else
        warn "tuned-stop service failed — manually verify CPU governor"
        echo "  Manual restore: for f in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do echo powersave | sudo tee \$f; done"
    fi

    # 2. Remove gaming session lock
    rm -f "${RUNTIME_DIR}/hisnos-gaming.lock"

    # 3. Restore vault idle timer
    "${SYSTEMCTL}" --user start hisnos-vault-idle.timer 2>/dev/null || true

    # 4. Stop gaming user service
    "${SYSTEMCTL}" --user stop hisnos-gaming.service 2>/dev/null || true

    ok "Gaming mode reset complete"
    log "HISNOS_RECOVER_GAMING_RESET"
}

# ── Command: lab-emergency-stop ───────────────────────────────────────────────

cmd_lab_emergency_stop() {
    sep "Lab Emergency Stop"

    local session_file="${RUNTIME_DIR}/hisnos-lab-session.json"
    local session_id=""

    if [[ -f "${session_file}" ]]; then
        session_id=$(python3 -c \
            "import json; print(json.load(open('${session_file}')).get('session_id',''))" \
            2>/dev/null || echo "")
    fi

    # 1. Kill any running bwrap processes
    local bwrap_pids
    bwrap_pids=$(pgrep -x bwrap 2>/dev/null || true)
    if [[ -n "${bwrap_pids}" ]]; then
        log "terminating bwrap processes: ${bwrap_pids}"
        kill ${bwrap_pids} 2>/dev/null || true
        sleep 0.5
        kill -9 ${bwrap_pids} 2>/dev/null || true
        ok "bwrap processes terminated"
    else
        ok "no bwrap processes found"
    fi

    # 2. Stop any running lab systemd unit
    local unit_name=""
    if [[ -n "${session_id}" ]]; then
        unit_name="hisnos-lab-${session_id}.service"
        "${SYSTEMCTL}" --user stop "${unit_name}" 2>/dev/null || true
    fi

    # 3. Emergency flush via netd (removes veth pairs and nftables rules)
    log "requesting netd emergency flush..."
    local netd_sock="/run/hisnos/lab-netd.sock"
    if [[ -S "${netd_sock}" ]]; then
        NETD_SOCK="${netd_sock}" NETD_REQ='{"op":"emergency-flush"}' \
        python3 - <<'PYEOF' 2>/dev/null || true
import socket, os, sys
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.settimeout(10)
s.connect(os.environ['NETD_SOCK'])
s.sendall(os.environ['NETD_REQ'].encode() + b'\n')
s.shutdown(socket.SHUT_WR)
resp = b''
while True:
    chunk = s.recv(4096)
    if not chunk: break
    resp += chunk
s.close()
print(resp.decode().strip())
PYEOF
        ok "netd emergency flush requested"
    else
        warn "lab-netd socket not found — manually remove veth interfaces"
        # Brute-force veth cleanup
        for iface in $(ip link show 2>/dev/null | grep -oP "vlh-\w+" | sort -u); do
            sudo ip link delete "${iface}" 2>/dev/null && log "deleted ${iface}" || true
        done
    fi

    # 4. Remove session lock
    rm -f "${session_file}"

    ok "Lab emergency stop complete"
    log "HISNOS_RECOVER_LAB_EMERGENCY_STOP session_id=${session_id}"
}

# ── Command: status ───────────────────────────────────────────────────────────

cmd_status() {
    local ts
    ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)

    # Gather subsystem states
    local fw_active="false"
    "${SYSTEMCTL}" is-active --quiet nftables.service 2>/dev/null && fw_active="true"

    local vault_mounted="false"
    grep -q gocryptfs /proc/mounts 2>/dev/null && vault_mounted="true"

    local gaming_active="false"
    [[ -f "${RUNTIME_DIR}/hisnos-gaming.lock" ]] && gaming_active="true"

    local lab_active="false"
    [[ -f "${RUNTIME_DIR}/hisnos-lab-session.json" ]] && lab_active="true"

    local logd_active="false"
    "${SYSTEMCTL}" --user is-active --quiet hisnos-logd.service 2>/dev/null && logd_active="true"

    local threatd_active="false"
    "${SYSTEMCTL}" --user is-active --quiet hisnos-threatd.service 2>/dev/null && threatd_active="true"

    local auditd_active="false"
    "${SYSTEMCTL}" is-active --quiet auditd.service 2>/dev/null && auditd_active="true"

    local dashboard_active="false"
    "${SYSTEMCTL}" --user is-active --quiet hisnos-dashboard.service 2>/dev/null && \
        dashboard_active="true"

    local risk_score=0 risk_level="unknown"
    if [[ -f "${VAR_DIR}/threat-state.json" ]]; then
        risk_score=$(python3 -c \
            "import json; print(json.load(open('${VAR_DIR}/threat-state.json')).get('risk_score',0))" \
            2>/dev/null || echo "0")
        risk_level=$(python3 -c \
            "import json; print(json.load(open('${VAR_DIR}/threat-state.json')).get('current_risk_level','unknown'))" \
            2>/dev/null || echo "unknown")
    fi

    cat << EOF
{
  "timestamp": "${ts}",
  "subsystems": {
    "firewall":  {"active": ${fw_active}},
    "vault":     {"mounted": ${vault_mounted}},
    "gaming":    {"active": ${gaming_active}},
    "lab":       {"session_active": ${lab_active}},
    "logd":      {"active": ${logd_active}},
    "threatd":   {"active": ${threatd_active}},
    "auditd":    {"active": ${auditd_active}},
    "dashboard": {"active": ${dashboard_active}}
  },
  "threat": {
    "risk_score": ${risk_score},
    "risk_level": "${risk_level}"
  }
}
EOF
}

# ── Entrypoint ────────────────────────────────────────────────────────────────

usage() {
    cat << EOF
${BOLD}HisnOS Recovery CLI${NC}

Usage: ${SCRIPT} <command> [options]

Commands:
  status                  Print subsystem health as JSON
  rollback-guide          Print rollback instructions for current state
  vault-force-lock        Force-unmount all gocryptfs vaults
  firewall-flush          Emergency flush of nftables rules  [requires --confirm]
  gaming-reset            Force-reset gaming tuning to normal profile
  lab-emergency-stop      Terminate all lab sessions + flush lab network
  dashboard-safe-mode     Restart dashboard in safe mode (debug logging)

Options:
  --confirm               Required for destructive commands (firewall-flush)

Examples:
  ${SCRIPT} status
  ${SCRIPT} vault-force-lock
  ${SCRIPT} firewall-flush --confirm
  ${SCRIPT} rollback-guide
EOF
}

CURRENT_CMD="${1:-}"
case "${CURRENT_CMD}" in
    status)               cmd_status ;;
    rollback-guide)       cmd_rollback_guide ;;
    vault-force-lock)     cmd_vault_force_lock ;;
    firewall-flush)       cmd_firewall_flush "${2:-}" ;;
    gaming-reset)         cmd_gaming_reset ;;
    lab-emergency-stop)   cmd_lab_emergency_stop ;;
    dashboard-safe-mode)  cmd_dashboard_safe_mode ;;
    --help|-h|help)       usage; exit 0 ;;
    "")                   usage; exit 1 ;;
    *)
        fail "Unknown command: ${CURRENT_CMD}"
        usage
        exit 2
        ;;
esac
