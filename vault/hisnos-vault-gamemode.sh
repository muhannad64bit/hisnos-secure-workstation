#!/usr/bin/env bash
# vault/hisnos-vault-gamemode.sh — HisnOS vault GameMode integration hook
#
# Called by GameMode when a game starts or stops.
# Stops the vault idle auto-lock timer during active gaming so the vault is
# not locked mid-session; re-enables the timer when gaming ends.
#
# Integration: reference this script from ~/.config/gamemode.ini:
#
#   [custom]
#   start=$HOME/.local/share/hisnos/vault/hisnos-vault-gamemode.sh start
#   end=$HOME/.local/share/hisnos/vault/hisnos-vault-gamemode.sh end
#
# GameMode runs start/end scripts as the game-launching user with the full
# session environment (DBUS_SESSION_BUS_ADDRESS, XDG_RUNTIME_DIR set).
#
# Design decisions:
#   - Screen-lock auto-lock is NOT disabled during gaming: if the user
#     manually locks their screen mid-game (alt+ctrl+L), the vault should
#     still lock. Only the idle timer is gated.
#   - Suspend auto-lock is NOT disabled: lid-close/suspend during gaming
#     must still lock the vault (suspend race mitigation).
#   - If systemctl --user fails (user not logged in via systemd), the
#     script logs a warning and exits 0 so GameMode is not blocked.
#
# Usage:
#   ./vault/hisnos-vault-gamemode.sh start   # called by GameMode on game start
#   ./vault/hisnos-vault-gamemode.sh end     # called by GameMode on game end
#   ./vault/hisnos-vault-gamemode.sh status  # show current timer state

set -euo pipefail

LOG_TAG="hisnos-vault-gamemode"
IDLE_TIMER="hisnos-vault-idle.timer"
CMD="${1:-status}"

# ── Logging ───────────────────────────────────────────────────────────────────
# Log to systemd journal via logger (available without session D-Bus)
log()  { logger -t "${LOG_TAG}" "$*" 2>/dev/null || echo "[${LOG_TAG}] $*" >&2; }
log_out() { echo "[${LOG_TAG}] $*"; }

# ── Ensure user systemd environment is reachable ──────────────────────────────
# GameMode may invoke this script before XDG_RUNTIME_DIR is set in PATH-derived
# environments. Detect and set if missing.
if [[ -z "${XDG_RUNTIME_DIR:-}" ]]; then
    XDG_RUNTIME_DIR="/run/user/$(id -u)"
    export XDG_RUNTIME_DIR
fi

if [[ -z "${DBUS_SESSION_BUS_ADDRESS:-}" ]]; then
    # Attempt to locate the session bus socket from the runtime dir
    BUS_SOCKET="${XDG_RUNTIME_DIR}/bus"
    if [[ -S "${BUS_SOCKET}" ]]; then
        DBUS_SESSION_BUS_ADDRESS="unix:path=${BUS_SOCKET}"
        export DBUS_SESSION_BUS_ADDRESS
    fi
fi

# ── Helper: run systemctl --user with error handling ─────────────────────────
user_systemctl() {
    local action="$1" unit="$2"
    if systemctl --user "${action}" "${unit}" 2>/dev/null; then
        return 0
    else
        log "WARNING: systemctl --user ${action} ${unit} failed"
        log "  XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-unset}"
        log "  DBUS_SESSION_BUS_ADDRESS=${DBUS_SESSION_BUS_ADDRESS:-unset}"
        return 1
    fi
}

# ── Command dispatch ──────────────────────────────────────────────────────────
case "${CMD}" in

start)
    # Game starting — stop idle timer to prevent mid-session vault lock
    log "GameMode start: stopping vault idle timer"

    if user_systemctl "is-active" "${IDLE_TIMER}" 2>/dev/null; then
        if user_systemctl "stop" "${IDLE_TIMER}"; then
            log "Vault idle timer stopped for gaming session"
        else
            log "WARNING: could not stop idle timer — vault may lock mid-session"
        fi
    else
        log "Vault idle timer was not active — nothing to stop"
    fi

    # Log current vault state so operator can audit
    LOCK_FILE="${XDG_RUNTIME_DIR}/hisnos-vault.lock"
    if [[ -f "${LOCK_FILE}" ]]; then
        MOUNT_TS=$(cut -d: -f2- "${LOCK_FILE}" 2>/dev/null || echo "unknown")
        log "Vault is MOUNTED (since: ${MOUNT_TS}) — idle lock suspended for gaming"
    else
        log "Vault is LOCKED — idle lock suspension is a no-op"
    fi
    ;;

end)
    # Game stopped — re-enable idle timer
    log "GameMode end: resuming vault idle timer"

    if user_systemctl "start" "${IDLE_TIMER}"; then
        log "Vault idle timer resumed"
    else
        log "WARNING: could not restart idle timer — vault will not auto-lock on idle"
        log "  Manual restart: systemctl --user start ${IDLE_TIMER}"
    fi

    # If vault is still mounted at game end, note it for the operator
    LOCK_FILE="${XDG_RUNTIME_DIR}/hisnos-vault.lock"
    if [[ -f "${LOCK_FILE}" ]]; then
        MOUNT_TS=$(cut -d: -f2- "${LOCK_FILE}" 2>/dev/null || echo "unknown")
        log "Vault is still MOUNTED after gaming session (mounted since: ${MOUNT_TS})"
        log "  It will auto-lock after 5 minutes of idle (timer restarted)"
    fi
    ;;

status)
    log_out "Vault idle timer state:"
    if systemctl --user is-active --quiet "${IDLE_TIMER}" 2>/dev/null; then
        log_out "  ${IDLE_TIMER}: ACTIVE (will lock vault after idle)"
    else
        log_out "  ${IDLE_TIMER}: INACTIVE (gaming session or manually stopped)"
    fi

    LOCK_FILE="${XDG_RUNTIME_DIR}/hisnos-vault.lock"
    if [[ -f "${LOCK_FILE}" ]]; then
        log_out "  Vault state: MOUNTED"
    else
        log_out "  Vault state: LOCKED"
    fi
    ;;

*)
    echo "Usage: ${0} <start|end|status>"
    echo ""
    echo "  start   Stop vault idle timer (called by GameMode on game start)"
    echo "  end     Resume vault idle timer (called by GameMode on game end)"
    echo "  status  Show current timer and vault state"
    exit 1
    ;;

esac
