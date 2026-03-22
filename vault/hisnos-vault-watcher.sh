#!/usr/bin/env bash
# vault/hisnos-vault-watcher.sh — HisnOS vault D-Bus auto-lock watcher
#
# Listens to D-Bus signals and locks the vault automatically when:
#   1. Screen is locked (org.freedesktop.ScreenSaver.ActiveChanged → true)
#   2. System suspends   (org.freedesktop.login1.Manager.PrepareForSleep → true)
#
# Intended to run as a user systemd service (see vault/systemd/hisnos-vault-watcher.service).
# Uses gdbus (part of glib2, always available on GNOME/KDE Fedora) for D-Bus monitoring.
#
# Suspend protection (systemd-inhibit delay lock):
#   For the PrepareForSleep trigger, the watcher wraps the vault lock call with
#   `systemd-inhibit --what=sleep --mode=delay`. This holds a delay inhibitor so
#   logind waits for fusermount to complete before proceeding with suspend.
#   Without this, a race exists where the kernel could enter S3 before the FUSE
#   mount is fully torn down if vault files are open.
#
#   The inhibitor is released automatically when `vault lock` exits (whether
#   fusermount succeeded, failed, or lazily unmounted).
#
#   Screen-lock events do NOT use an inhibitor — no sleep transition is in
#   progress, so there is no race to close.
#
# Fallback detection order:
#   1. gdbus (glib2-devel / glib2 — always present on Kinoite)
#   2. dbus-monitor (dbus-tools — fallback)
#
# Usage:
#   # As a service (preferred):
#   systemctl --user enable --now hisnos-vault-watcher.service
#
#   # Manually for testing:
#   ./vault/hisnos-vault-watcher.sh
#   ./vault/hisnos-vault-watcher.sh --dry-run      # print signals but don't lock
#   ./vault/hisnos-vault-watcher.sh --no-inhibit   # skip systemd-inhibit (debugging)
#
# Signal specification:
#   ScreenSaver: session bus, org.freedesktop.ScreenSaver iface, ActiveChanged signal
#   PrepareForSleep: system bus, org.freedesktop.login1.Manager iface, PrepareForSleep signal
#
# This script runs persistently (does not exit). The systemd service handles
# restart-on-failure.

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
VAULT_SCRIPT="${HISNOS_VAULT_SCRIPT:-$(dirname "$(realpath "$0")")/hisnos-vault.sh}"
LOG_TAG="hisnos-vault-watcher"
DRY_RUN=false
USE_INHIBIT=true   # use systemd-inhibit delay lock on suspend path

for arg in "$@"; do
    case "${arg}" in
        --dry-run)    DRY_RUN=true ;;
        --no-inhibit) USE_INHIBIT=false ;;
    esac
done

# ── Logging ───────────────────────────────────────────────────────────────────
# Log to systemd journal via stderr (systemd captures fd2)
log()  { echo "[${LOG_TAG}] $*" >&2; }
log_dry() { echo "[${LOG_TAG}] [DRY-RUN] $*" >&2; }

# ── Validate vault script ──────────────────────────────────────────────────────
if [[ ! -x "${VAULT_SCRIPT}" ]]; then
    # Try PATH lookup
    if command -v hisnos-vault &>/dev/null; then
        VAULT_SCRIPT="hisnos-vault"
    else
        log "ERROR: vault script not found at ${VAULT_SCRIPT}"
        log "Set HISNOS_VAULT_SCRIPT env var or ensure hisnos-vault.sh is executable"
        exit 1
    fi
fi

# ── Lock triggers ─────────────────────────────────────────────────────────────

# _vault_lock_exec: internal — run vault lock, optionally under inhibitor
# Usage: _vault_lock_exec <reason> [inhibit=true|false]
_vault_lock_exec() {
    local reason="$1"
    local inhibit="${2:-false}"

    if "${VAULT_SCRIPT}" lock; then
        log "Vault locked successfully (trigger: ${reason})"
        # Emit telemetry signal for dashboard / audit pipeline
        logger -t "hisnos-vault" "VAULT_LOCKED trigger=${reason}" 2>/dev/null || true
    else
        log "WARNING: vault lock failed (trigger: ${reason}) — check vault state"
        logger -t "hisnos-vault" "VAULT_LOCK_FAILED trigger=${reason}" 2>/dev/null || true
    fi
}

# do_lock: standard lock — used for screen-lock events (no sleep inhibitor needed)
do_lock() {
    local reason="$1"
    if [[ "${DRY_RUN}" == "true" ]]; then
        log_dry "Would lock vault (trigger: ${reason})"
        return
    fi

    log "Auto-lock triggered: ${reason}"

    # Check if vault is actually mounted before attempting lock
    XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
    LOCK_FILE="${XDG_RUNTIME_DIR}/hisnos-vault.lock"

    if [[ ! -f "${LOCK_FILE}" ]]; then
        log "Vault already locked — skipping lock (${reason})"
        return
    fi

    _vault_lock_exec "${reason}"
}

# do_lock_inhibited: suspend-safe lock — holds a systemd delay inhibitor so
# logind waits for fusermount to complete before entering S3.
# This closes the suspend race window identified in cross-subsystem analysis.
do_lock_inhibited() {
    local reason="$1"
    if [[ "${DRY_RUN}" == "true" ]]; then
        log_dry "Would lock vault with inhibitor (trigger: ${reason})"
        return
    fi

    log "Auto-lock triggered: ${reason} (suspend-safe path)"

    XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
    LOCK_FILE="${XDG_RUNTIME_DIR}/hisnos-vault.lock"

    if [[ ! -f "${LOCK_FILE}" ]]; then
        log "Vault already locked — skipping lock (${reason})"
        return
    fi

    # Use systemd-inhibit delay lock if available and not suppressed
    if [[ "${USE_INHIBIT}" == "true" ]] && command -v systemd-inhibit &>/dev/null; then
        log "Taking sleep delay inhibitor for vault lock (suspend race protection)"
        # systemd-inhibit runs the command, holds the inhibitor while it runs,
        # then releases it when the command exits — exactly what we need.
        if systemd-inhibit \
                --what=sleep \
                --mode=delay \
                --who="hisnos-vault-watcher" \
                --why="Locking vault before suspend" \
                "${VAULT_SCRIPT}" lock; then
            log "Vault locked successfully (trigger: ${reason}, inhibitor released)"
            logger -t "hisnos-vault" "VAULT_LOCKED trigger=${reason} inhibitor=true" 2>/dev/null || true
        else
            log "WARNING: vault lock failed under inhibitor (trigger: ${reason})"
            logger -t "hisnos-vault" "VAULT_LOCK_FAILED trigger=${reason} inhibitor=true" 2>/dev/null || true
        fi
    else
        # Fallback: lock without inhibitor (original behaviour)
        [[ "${USE_INHIBIT}" == "false" ]] && log "Inhibitor disabled by --no-inhibit flag"
        command -v systemd-inhibit &>/dev/null || log "systemd-inhibit not found — locking without inhibitor"
        _vault_lock_exec "${reason}"
    fi
}

# ── D-Bus monitor implementation ───────────────────────────────────────────────
# Strategy: run two background gdbus monitor processes, one on session bus
# (ScreenSaver) and one on system bus (PrepareForSleep). Parse output from
# both via a shared FIFO pipe.
#
# gdbus monitor output format:
#   /org/freedesktop/ScreenSaver: org.freedesktop.ScreenSaver.ActiveChanged (true,)
#   /org/freedesktop/login1: org.freedesktop.login1.Manager.PrepareForSleep (true,)
#
# Note: dbus-monitor is a fallback; gdbus is preferred as it's always present.

FIFO_PATH="/tmp/hisnos-vault-watcher-$$.fifo"
trap 'rm -f "${FIFO_PATH}"; kill 0' EXIT INT TERM

mkfifo "${FIFO_PATH}"

start_monitors() {
    if command -v gdbus &>/dev/null; then
        log "Using gdbus for D-Bus monitoring"

        # Session bus: ScreenSaver signals
        gdbus monitor \
            --session \
            --dest org.freedesktop.ScreenSaver \
            --object-path /org/freedesktop/ScreenSaver \
            >> "${FIFO_PATH}" 2>&1 &
        GDBUS_SESSION_PID=$!

        # System bus: PrepareForSleep signals
        gdbus monitor \
            --system \
            --dest org.freedesktop.login1 \
            --object-path /org/freedesktop/login1 \
            >> "${FIFO_PATH}" 2>&1 &
        GDBUS_SYSTEM_PID=$!

        log "Session bus monitor PID: ${GDBUS_SESSION_PID}"
        log "System bus monitor PID:  ${GDBUS_SYSTEM_PID}"

    elif command -v dbus-monitor &>/dev/null; then
        log "gdbus not found — falling back to dbus-monitor"

        # Session bus (ScreenSaver)
        dbus-monitor --session \
            "type='signal',interface='org.freedesktop.ScreenSaver',member='ActiveChanged'" \
            >> "${FIFO_PATH}" 2>&1 &

        # System bus (PrepareForSleep) — requires access to system bus
        dbus-monitor --system \
            "type='signal',interface='org.freedesktop.login1.Manager',member='PrepareForSleep'" \
            >> "${FIFO_PATH}" 2>&1 &

        log "dbus-monitor started (session + system buses)"
    else
        log "ERROR: neither gdbus nor dbus-monitor found"
        log "Install: sudo rpm-ostree install glib2"
        exit 1
    fi
}

# ── KDE-specific: kscreenlocker_greet detection ───────────────────────────────
# KDE Plasma 6 uses org.kde.screensaver, not org.freedesktop.ScreenSaver in all cases.
# Add a KDE-specific monitor on session bus.
start_kde_monitor() {
    if command -v gdbus &>/dev/null; then
        # KDE Plasma 6: kscreenlocker uses org.freedesktop.ScreenSaver on session bus
        # but also signals via org.kde.screensaver — monitor both just in case
        gdbus monitor \
            --session \
            --dest org.kde.screensaver \
            --object-path /ScreenSaver \
            >> "${FIFO_PATH}" 2>&1 &
        KDE_PID=$!
        log "KDE screensaver monitor PID: ${KDE_PID} (org.kde.screensaver)"
    fi
}

# ── Main event loop ───────────────────────────────────────────────────────────
log "Starting vault auto-lock watcher"
log "Vault script:    ${VAULT_SCRIPT}"
log "Sleep inhibitor: ${USE_INHIBIT} (--no-inhibit to disable)"
[[ "${DRY_RUN}" == "true" ]] && log "DRY-RUN mode — no locks will be performed"

start_monitors
start_kde_monitor 2>/dev/null || true  # KDE monitor is optional

log "Listening for lock/sleep signals..."

# Read signal output from FIFO and react to lock/sleep events
while IFS= read -r line; do
    # ── ScreenSaver locked (ActiveChanged: true) ──────────────────────────────
    # gdbus:       "ActiveChanged (true,)"  or "ActiveChanged (true)"
    # dbus-monitor: various formats, but "boolean true" in the body
    if echo "${line}" | grep -qiE "(ActiveChanged|ScreenSaverActivated).*(true)" 2>/dev/null; then
        do_lock "screen-lock"
        continue
    fi

    # KDE kscreenlocker: ActiveChanged signal (same pattern)
    if echo "${line}" | grep -qiE "kscreenlocker.*ActiveChanged.*(true)" 2>/dev/null; then
        do_lock "kde-screen-lock"
        continue
    fi

    # ── PrepareForSleep (suspend) ─────────────────────────────────────────────
    # gdbus:       "PrepareForSleep (true,)"
    # dbus-monitor: "boolean true" after member=PrepareForSleep
    # Uses inhibitor lock to close the suspend race window.
    if echo "${line}" | grep -qiE "PrepareForSleep.*(true)" 2>/dev/null; then
        do_lock_inhibited "suspend"
        continue
    fi

    # ── Debug: uncomment to trace all received signals ────────────────────────
    # log "DBG: ${line}"

done < "${FIFO_PATH}"

log "Event loop exited — watcher stopped"
