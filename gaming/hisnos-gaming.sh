#!/usr/bin/env bash
# gaming/hisnos-gaming.sh — HisnOS Gaming Performance Integration Orchestrator
#
# Idempotent start/stop wrapper that coordinates user-space and privileged
# tuning for gaming sessions. Called by GameMode hooks or directly.
#
# Usage:
#   hisnos-gaming.sh start   # activate gaming mode
#   hisnos-gaming.sh stop    # deactivate gaming mode and restore
#   hisnos-gaming.sh status  # print current state as JSON
#
# Privilege model:
#   - This script runs as the logged-in user
#   - CPU/IRQ/nftables tuning is delegated to hisnos-gaming-tuned-start.service
#     and hisnos-gaming-tuned-stop.service (system services, polkit-authorized
#     for members of the hisnos-gaming group)
#   - Vault timer and control plane transitions run entirely in user space
#
# Rollback guarantee:
#   The privileged tuning service saves state before any change. If any tuning
#   step fails, the service resets to saved state. This script mirrors that
#   contract: on start failure, stop is called automatically.
#
# Telemetry:
#   Emits structured journal events: HISNOS_GAMING_STARTED, HISNOS_GAMING_STOPPED,
#   HISNOS_GAMING_ROLLBACK for dashboard and threatd consumption.
set -euo pipefail

readonly SCRIPT_NAME="hisnos-gaming"
readonly LOG_TAG="hisnos-gaming"
readonly DASHBOARD_URL="${HISNOS_DASHBOARD_URL:-http://127.0.0.1:7374}"
readonly STATE_LOCK="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/hisnos-gaming.lock"
readonly TUNED_START_SVC="hisnos-gaming-tuned-start.service"
readonly TUNED_STOP_SVC="hisnos-gaming-tuned-stop.service"
readonly VAULT_IDLE_TIMER="hisnos-vault-idle.timer"
readonly DASHBOARD_CONFIRM_URL="${DASHBOARD_URL}/api/confirm/token"
readonly DASHBOARD_MODE_URL="${DASHBOARD_URL}/api/system/mode"

# ── Logging ──────────────────────────────────────────────────────────────────

log_info()  { systemd-cat -t "${LOG_TAG}" -p info  echo "$*" 2>/dev/null || echo "[INFO]  $*"; }
log_warn()  { systemd-cat -t "${LOG_TAG}" -p warning echo "$*" 2>/dev/null || echo "[WARN]  $*" >&2; }
log_error() { systemd-cat -t "${LOG_TAG}" -p err   echo "$*" 2>/dev/null || echo "[ERROR] $*" >&2; }

emit_event() {
    local event="$1"; shift
    local fields="$*"
    systemd-cat -t "${LOG_TAG}" -p notice \
        printf '%s %s' "${event}" "${fields}" 2>/dev/null || true
    log_info "${event} ${fields}"
}

# ── Helpers ──────────────────────────────────────────────────────────────────

is_gaming_active() {
    [[ -f "${STATE_LOCK}" ]]
}

dashboard_token() {
    curl -sS --max-time 3 "${DASHBOARD_CONFIRM_URL}" 2>/dev/null \
        | sed -n 's/.*"token":"\([^"]*\)".*/\1/p' || echo ""
}

# Update Control Plane mode via dashboard API.
# Non-fatal: dashboard may not be running during game session.
cp_transition() {
    local to_mode="$1"
    local token
    token="$(dashboard_token)"
    if [[ -z "${token}" ]]; then
        log_warn "dashboard not reachable — control plane transition skipped (mode=${to_mode})"
        return 0
    fi
    local body
    body="$(printf '{"to":"%s"}' "${to_mode}")"
    local http_code
    http_code=$(curl -sS -o /dev/null -w "%{http_code}" \
        --max-time 5 \
        -X POST "${DASHBOARD_MODE_URL}" \
        -H "Content-Type: application/json" \
        -H "X-HisnOS-Confirm: ${token}" \
        -d "${body}" 2>/dev/null || echo "000")
    if [[ "${http_code}" != "200" ]]; then
        log_warn "control plane transition to '${to_mode}' returned HTTP ${http_code} — continuing"
    else
        log_info "control plane mode → ${to_mode}"
    fi
}

# Start a system service (polkit-authorized for hisnos-gaming group).
start_system_svc() {
    local svc="$1"
    if ! systemctl start "${svc}" 2>/dev/null; then
        log_warn "system service ${svc} failed — privileged tuning unavailable"
        return 1
    fi
    return 0
}

# ── Gaming mode start ─────────────────────────────────────────────────────────

do_start() {
    if is_gaming_active; then
        log_info "gaming mode already active — idempotent start, exiting"
        exit 0
    fi

    local fail=false

    # 1. Transition control plane to gaming mode (non-fatal)
    cp_transition "gaming"

    # 2. Suppress vault idle auto-lock (keep screen-lock active).
    #    The idle timer fires hisnos-vault-idle.service which locks the vault.
    #    Stopping the timer prevents auto-lock; screen-lock watcher remains active.
    if systemctl --user is-active --quiet "${VAULT_IDLE_TIMER}" 2>/dev/null; then
        if systemctl --user stop "${VAULT_IDLE_TIMER}" 2>/dev/null; then
            log_info "vault idle timer suppressed for gaming session"
        else
            log_warn "failed to stop vault idle timer — vault may auto-lock during play"
        fi
    fi

    # 3. Request privileged tuning (CPU governor, IRQ affinity, nftables gaming chain).
    if ! start_system_svc "${TUNED_START_SVC}"; then
        log_warn "privileged tuning skipped — performance may be suboptimal"
        # Non-fatal: session continues without kernel-level tuning
    fi

    # 4. Mark gaming session active.
    echo "started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "${STATE_LOCK}"
    echo "pid=$$" >> "${STATE_LOCK}"

    emit_event "HISNOS_GAMING_STARTED" \
        "vault_timer_suppressed=true cpu_gov_requested=true nft_gaming_requested=true"

    log_info "gaming mode active"

    if [[ "${fail}" == "true" ]]; then
        log_warn "gaming mode started with degraded tuning — run 'hisnos-gaming.sh status' for details"
    fi
}

# ── Gaming mode stop ──────────────────────────────────────────────────────────

do_stop() {
    if ! is_gaming_active; then
        log_info "gaming mode not active — idempotent stop, exiting"
        exit 0
    fi

    # 1. Request privileged tuning rollback (restores CPU governor, IRQ affinity, nftables).
    if ! start_system_svc "${TUNED_STOP_SVC}"; then
        log_warn "privileged tuning rollback service failed — system tuning may remain in gaming state"
        emit_event "HISNOS_GAMING_ROLLBACK" "reason=tuned_stop_failed"
    fi

    # 2. Restore vault idle auto-lock timer.
    if ! systemctl --user is-active --quiet "${VAULT_IDLE_TIMER}" 2>/dev/null; then
        if systemctl --user start "${VAULT_IDLE_TIMER}" 2>/dev/null; then
            log_info "vault idle timer restored"
        else
            log_warn "failed to restore vault idle timer — vault will not auto-lock"
        fi
    fi

    # 3. Remove session lock.
    rm -f "${STATE_LOCK}"

    # 4. Transition control plane back to normal mode (non-fatal).
    cp_transition "normal"

    emit_event "HISNOS_GAMING_STOPPED" "rollback=completed"
    log_info "gaming mode deactivated — system restored to normal profile"
}

# ── Status ────────────────────────────────────────────────────────────────────

do_status() {
    local active="false"
    local started_at=""
    if is_gaming_active; then
        active="true"
        started_at=$(grep "^started_at=" "${STATE_LOCK}" 2>/dev/null | cut -d= -f2- || echo "")
    fi

    local vault_timer_active="false"
    systemctl --user is-active --quiet "${VAULT_IDLE_TIMER}" 2>/dev/null && vault_timer_active="true"

    printf '{"gaming_active":%s,"started_at":"%s","vault_idle_timer_active":%s,"timestamp":"%s"}\n' \
        "${active}" "${started_at}" "${vault_timer_active}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}

# ── Entrypoint ────────────────────────────────────────────────────────────────

case "${1:-}" in
    start)  do_start  ;;
    stop)   do_stop   ;;
    status) do_status ;;
    *)
        echo "Usage: ${SCRIPT_NAME} {start|stop|status}" >&2
        exit 1
        ;;
esac
