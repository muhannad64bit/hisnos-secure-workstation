#!/usr/bin/env bash
# vault/hisnos-vault.sh — HisnOS gocryptfs vault CLI
#
# Manages an AES-256-GCM encrypted vault using gocryptfs.
# Key material exists ONLY in RAM while the vault is mounted.
# Cipher directory is on disk; plaintext mount is FUSE-based and
# is automatically unmounted on screen lock, idle, or suspend.
#
# Architecture:
#   CIPHER_DIR   (~/.local/share/hisnos/vault-cipher)  — encrypted content on disk
#   MOUNT_DIR    (~/.local/share/hisnos/vault-mount)   — plaintext FUSE mountpoint
#   LOCK_FILE    (/run/user/<UID>/hisnos-vault.lock)   — ephemeral: tracks mounted state
#
# Commands:
#   init         — initialise a new vault (one-time setup)
#   mount        — unlock and mount vault (prompts for passphrase)
#   lock         — unmount and lock vault
#   status       — show vault state (mounted / locked / uninitialised)
#   rotate-key   — change vault passphrase
#   check        — verify vault integrity (gocryptfs -fsck)
#
# Security properties:
#   - AES-256-GCM authenticated encryption (gocryptfs default)
#   - Scrypt KDF: N=65536, r=8, p=1 (default; increase for higher security)
#   - No swap — vault keys never touch disk (ensure no disk swap: hisnos-validate.sh check 21)
#   - Auto-lock via D-Bus watcher (hisnos-vault-watcher.sh) and idle timer
#   - Passphrase never stored — only in memory during gocryptfs operation
#
# Usage:
#   ./vault/hisnos-vault.sh init
#   ./vault/hisnos-vault.sh mount
#   ./vault/hisnos-vault.sh lock
#   ./vault/hisnos-vault.sh status
#   ./vault/hisnos-vault.sh rotate-key
#   ./vault/hisnos-vault.sh check
#
# Environment overrides:
#   HISNOS_CIPHER_DIR  — override cipher directory path
#   HISNOS_MOUNT_DIR   — override mount directory path

set -euo pipefail

# ── Paths ──────────────────────────────────────────────────────────────────────
XDG_DATA_HOME="${XDG_DATA_HOME:-${HOME}/.local/share}"
HISNOS_STATE="${XDG_DATA_HOME}/hisnos"

CIPHER_DIR="${HISNOS_CIPHER_DIR:-${HISNOS_STATE}/vault-cipher}"
MOUNT_DIR="${HISNOS_MOUNT_DIR:-${HISNOS_STATE}/vault-mount}"

# Lock file lives in /run/user/<UID> — ephemeral tmpfs, cleared on logout/reboot
XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
LOCK_FILE="${XDG_RUNTIME_DIR}/hisnos-vault.lock"

# ── Colours ────────────────────────────────────────────────────────────────────
RED='\\033[0;31m'; GREEN='\\033[0;32m'; YELLOW='\\033[1;33m'
BOLD='\\033[1m'; DIM='\\033[2m'; NC='\\033[0m'

info()    { echo -e "${GREEN}[vault]${NC} $*"; }
warn()    { echo -e "${YELLOW}[vault WARN]${NC} $*"; }
error()   { echo -e "${RED}[vault ERROR]${NC} $*" >&2; exit 1; }
section() { echo -e "\\n${BOLD}── $* ──${NC}"; }

# ── Guard: must not run as root ───────────────────────────────────────────────
[[ "${EUID}" -ne 0 ]] || error "Do not run vault operations as root."

# ── Dependency check ──────────────────────────────────────────────────────────
require_cmd() {
    command -v "$1" &>/dev/null || error "Required command not found: $1 — install: sudo rpm-ostree install $2"
}

require_gocryptfs() {
    require_cmd gocryptfs gocryptfs
}

# ── State helpers ──────────────────────────────────────────────────────────────
is_initialised() {
    [[ -f "${CIPHER_DIR}/gocryptfs.conf" ]]
}

is_mounted() {
    # Check FUSE mount — both the lock file and the actual mount must be present
    [[ -f "${LOCK_FILE}" ]] && mountpoint -q "${MOUNT_DIR}" 2>/dev/null
}

mark_mounted() {
    echo "mounted:$(date -Iseconds)" > "${LOCK_FILE}"
}

mark_locked() {
    rm -f "${LOCK_FILE}"
}

# ── Command: init ─────────────────────────────────────────────────────────────
cmd_init() {
    require_gocryptfs

    section "Initialising HisnOS vault"

    if is_initialised; then
        error "Vault already initialised at: ${CIPHER_DIR}
  To reinitialise: remove ${CIPHER_DIR} first (THIS DESTROYS ALL VAULT DATA)
  To change passphrase: ${0} rotate-key"
    fi

    # Warn about swap — vault security depends on no disk swap
    SWAP_TYPE=$(swapon --show --noheadings --output TYPE 2>/dev/null | head -1 || echo "")
    if [[ "${SWAP_TYPE}" == "partition" || "${SWAP_TYPE}" == "file" ]]; then
        warn "DISK SWAP IS ACTIVE: ${SWAP_TYPE}"
        warn "Vault passphrase and key material may be written to disk!"
        warn "Run kernel/hisnos-validate.sh to verify: check 21 (no disk swap) should PASS"
        echo ""
        read -r -p "Continue anyway? [y/N] " CONFIRM
        [[ "${CONFIRM}" =~ ^[Yy]$ ]] || { info "Aborted."; exit 0; }
    fi

    # Ensure directories exist
    mkdir -p "${CIPHER_DIR}" "${MOUNT_DIR}"
    chmod 700 "${CIPHER_DIR}" "${MOUNT_DIR}"

    echo ""
    echo -e "  ${BOLD}Vault location:${NC} ${CIPHER_DIR}"
    echo -e "  ${BOLD}Mount point:   ${NC} ${MOUNT_DIR}"
    echo ""
    echo "  You will be prompted for a passphrase twice."
    echo "  Choose a strong passphrase — it cannot be recovered if lost."
    echo ""

    # gocryptfs init — user is prompted for passphrase interactively
    if gocryptfs -init \
        -scryptn 17 \
        "${CIPHER_DIR}"; then
        info "Vault initialised successfully."
        info "Mount with: ${0} mount"
        echo ""
        echo -e "  ${DIM}Cipher dir: ${CIPHER_DIR}${NC}"
        echo -e "  ${DIM}Config:     ${CIPHER_DIR}/gocryptfs.conf${NC}"
        echo -e "  ${DIM}Algorithm:  AES-256-GCM (gocryptfs default)${NC}"
    else
        error "gocryptfs init failed — check output above"
    fi
}

# ── Command: mount ─────────────────────────────────────────────────────────────
cmd_mount() {
    require_gocryptfs

    is_initialised || error "Vault not initialised. Run: ${0} init"

    if is_mounted; then
        info "Vault is already mounted at: ${MOUNT_DIR}"
        exit 0
    fi

    # Ensure mount directory exists and is empty
    mkdir -p "${MOUNT_DIR}"
    chmod 700 "${MOUNT_DIR}"

    # Verify mount dir is empty (not already a mountpoint with something else)
    if mountpoint -q "${MOUNT_DIR}" 2>/dev/null; then
        error "Mount point ${MOUNT_DIR} has an unexpected mountpoint — investigate before mounting vault"
    fi

    section "Unlocking vault"
    echo ""
    echo -e "  ${BOLD}Cipher:${NC} ${CIPHER_DIR}"
    echo -e "  ${BOLD}Mount: ${NC} ${MOUNT_DIR}"
    echo ""

    # gocryptfs mount — prompts for passphrase, then daemonises
    if gocryptfs \
        -nosyslog \
        "${CIPHER_DIR}" "${MOUNT_DIR}"; then
        mark_mounted
        info "Vault mounted at: ${MOUNT_DIR}"
        echo ""
        echo -e "  ${DIM}Lock with: ${0} lock${NC}"
        echo -e "  ${DIM}Auto-lock: screen lock, idle (5min), or suspend${NC}"
    else
        error "gocryptfs mount failed — check passphrase and cipher directory"
    fi
}

# ── Command: lock ─────────────────────────────────────────────────────────────
cmd_lock() {
    if ! is_mounted; then
        info "Vault is not mounted (already locked)."
        mark_locked
        exit 0
    fi

    section "Locking vault"

    # Unmount FUSE filesystem — this zeroes the in-kernel key cache
    if fusermount3 -u "${MOUNT_DIR}" 2>/dev/null || \
       fusermount -u  "${MOUNT_DIR}" 2>/dev/null || \
       umount "${MOUNT_DIR}" 2>/dev/null; then
        mark_locked
        info "Vault locked. Key material removed from memory."
    else
        # Try lazy unmount if processes have files open
        warn "Standard unmount failed — attempting lazy unmount (files may be in use)"
        if fusermount3 -uz "${MOUNT_DIR}" 2>/dev/null || \
           fusermount  -uz "${MOUNT_DIR}" 2>/dev/null; then
            mark_locked
            warn "Vault lazily unmounted — processes that had files open will receive errors"
            warn "Check for open file handles: lsof +D ${MOUNT_DIR}"
        else
            error "Could not unmount vault.
  Processes with open files: lsof +D ${MOUNT_DIR}
  Force: sudo umount -l ${MOUNT_DIR} (last resort)"
        fi
    fi
}

# ── Command: status ───────────────────────────────────────────────────────────
cmd_status() {
    section "HisnOS Vault Status"
    echo ""
    echo -e "  Cipher dir:   ${CIPHER_DIR}"
    echo -e "  Mount point:  ${MOUNT_DIR}"
    echo -e "  Lock file:    ${LOCK_FILE}"
    echo ""

    if ! is_initialised; then
        echo -e "  State: ${YELLOW}UNINITIALISED${NC}"
        echo ""
        echo -e "  Initialise with: ${0} init"
        return
    fi

    # Show gocryptfs config details
    if [[ -f "${CIPHER_DIR}/gocryptfs.conf" ]]; then
        CREATOR=$(python3 -c "
import json, sys
try:
    d = json.load(open('${CIPHER_DIR}/gocryptfs.conf'))
    c = d.get('Creator','unknown')
    v = d.get('Version', '?')
    print(f'gocryptfs v{v} | created by: {c}')
except Exception as e:
    print('(config parse error)')
" 2>/dev/null || echo "(config readable)")
        echo -e "  Config: ${DIM}${CREATOR}${NC}"
    fi

    if is_mounted; then
        echo -e "  State: ${GREEN}MOUNTED (UNLOCKED)${NC}"
        LOCK_TS=$(cat "${LOCK_FILE}" 2>/dev/null | cut -d: -f2- || echo "unknown")
        echo -e "  Mounted since: ${DIM}${LOCK_TS}${NC}"
        echo ""
        # Show mount usage
        USAGE=$(df -h "${MOUNT_DIR}" 2>/dev/null | tail -1 | awk '{print $3 " used of " $2 " (" $5 ")"}' || echo "?")
        echo -e "  Usage: ${USAGE}"
        echo ""
        echo -e "  ${DIM}Lock:   ${0} lock${NC}"
    else
        echo -e "  State: ${BOLD}LOCKED${NC}"
        echo ""
        # Cipher dir size (encrypted content on disk)
        if [[ -d "${CIPHER_DIR}" ]]; then
            CIPHER_SIZE=$(du -sh "${CIPHER_DIR}" 2>/dev/null | cut -f1 || echo "?")
            echo -e "  Encrypted content size: ${CIPHER_SIZE}"
        fi
        echo ""
        echo -e "  ${DIM}Unlock: ${0} mount${NC}"
    fi

    # Auto-lock status
    echo ""
    echo -e "  ${DIM}Auto-lock watcher:${NC}"
    if systemctl --user is-active --quiet hisnos-vault-watcher.service 2>/dev/null; then
        echo -e "    ${GREEN}●${NC} hisnos-vault-watcher.service: active"
    else
        echo -e "    ${YELLOW}○${NC} hisnos-vault-watcher.service: inactive"
        echo -e "      Enable: systemctl --user enable --now hisnos-vault-watcher.service"
    fi
    if systemctl --user is-active --quiet hisnos-vault-idle.timer 2>/dev/null; then
        echo -e "    ${GREEN}●${NC} hisnos-vault-idle.timer: active"
    else
        echo -e "    ${YELLOW}○${NC} hisnos-vault-idle.timer: inactive"
    fi
}

# ── Command: rotate-key ───────────────────────────────────────────────────────
cmd_rotate_key() {
    require_gocryptfs

    is_initialised || error "Vault not initialised. Run: ${0} init"

    if is_mounted; then
        error "Lock vault before rotating the key: ${0} lock"
    fi

    section "Rotating vault passphrase"
    echo ""
    warn "You will be prompted for the CURRENT passphrase, then the NEW passphrase twice."
    warn "If you lose the new passphrase, the vault data is unrecoverable."
    echo ""

    # gocryptfs -passwd changes the master key encryption passphrase
    if gocryptfs -passwd "${CIPHER_DIR}"; then
        info "Passphrase rotated successfully."
        info "The encrypted content is unaffected — only the key-wrapping passphrase changed."
    else
        error "Passphrase rotation failed — check output above"
    fi
}

# ── Command: check ────────────────────────────────────────────────────────────
cmd_check() {
    require_gocryptfs

    is_initialised || error "Vault not initialised."

    section "Vault integrity check"
    echo ""

    if is_mounted; then
        warn "Vault is currently mounted — fsck may produce false positives on a live mount"
        warn "Recommended: lock first with: ${0} lock"
        echo ""
        read -r -p "Continue anyway? [y/N] " CONFIRM
        [[ "${CONFIRM}" =~ ^[Yy]$ ]] || { info "Aborted."; exit 0; }
    fi

    echo "  Running gocryptfs -fsck (will prompt for passphrase)..."
    echo ""

    if gocryptfs -fsck "${CIPHER_DIR}"; then
        info "Integrity check passed — no errors found."
    else
        warn "gocryptfs fsck reported issues — check output above"
        warn "If file count mismatches are reported, this may be normal (gocryptfs.conf, gocryptfs.diriv)"
    fi
}

# ── Command: telemetry ────────────────────────────────────────────────────────
cmd_telemetry() {
    # Vault exposure telemetry: mounted duration, suspend-while-mounted events,
    # forced lazy unmount detection.
    # Designed for dashboard integration — outputs structured key=value lines
    # suitable for both human reading and machine parsing.

    section "Vault Exposure Telemetry"
    echo ""

    # ── Mounted duration ──────────────────────────────────────────────────────
    if is_mounted && [[ -f "${LOCK_FILE}" ]]; then
        MOUNT_TS_RAW=$(cut -d: -f2- "${LOCK_FILE}" 2>/dev/null || echo "")
        if [[ -n "${MOUNT_TS_RAW}" ]]; then
            MOUNT_EPOCH=$(date -d "${MOUNT_TS_RAW}" +%s 2>/dev/null || echo 0)
            NOW_EPOCH=$(date +%s)
            DURATION_SEC=$(( NOW_EPOCH - MOUNT_EPOCH ))
            DURATION_H=$(( DURATION_SEC / 3600 ))
            DURATION_M=$(( (DURATION_SEC % 3600) / 60 ))
            DURATION_S=$(( DURATION_SEC % 60 ))
            printf "  mounted_duration:  %02dh %02dm %02ds\n" "${DURATION_H}" "${DURATION_M}" "${DURATION_S}"
            printf "  mounted_since:     %s\n" "${MOUNT_TS_RAW}"
            # Warn if mounted longer than 8 hours
            if [[ ${DURATION_SEC} -gt 28800 ]]; then
                warn "Vault has been mounted for over 8 hours — consider locking"
            fi
        fi
    else
        echo "  mounted_duration:  0 (vault is locked)"
    fi
    echo ""

    # ── Suspend-while-mounted detection ───────────────────────────────────────
    # Look for PrepareForSleep events in the journal since the vault was last mounted.
    # If any occurred while the vault was mounted, the suspend race may have applied.
    SUSPEND_EVENTS=0
    LOCK_SUCCESSES_SUSPEND=0

    if [[ -f "${LOCK_FILE}" ]]; then
        MOUNT_TS_RAW=$(cut -d: -f2- "${LOCK_FILE}" 2>/dev/null || echo "")
        if [[ -n "${MOUNT_TS_RAW}" ]]; then
            # Count suspends since vault was mounted
            SUSPEND_EVENTS=$(journalctl --since="${MOUNT_TS_RAW}" \
                -g "PrepareForSleep" --no-pager 2>/dev/null | grep -c "true" || echo 0)
            # Count vault lock events triggered by suspend since vault was mounted
            LOCK_SUCCESSES_SUSPEND=$(journalctl --since="${MOUNT_TS_RAW}" \
                -t "hisnos-vault" -g "VAULT_LOCKED.*trigger=suspend" --no-pager 2>/dev/null \
                | wc -l || echo 0)
        fi
    fi

    if [[ ${SUSPEND_EVENTS} -gt 0 ]]; then
        echo "  suspend_events_since_mount: ${SUSPEND_EVENTS}"
        echo "  vault_locked_on_suspend:    ${LOCK_SUCCESSES_SUSPEND}"
        if [[ ${LOCK_SUCCESSES_SUSPEND} -lt ${SUSPEND_EVENTS} ]]; then
            MISSED=$(( SUSPEND_EVENTS - LOCK_SUCCESSES_SUSPEND ))
            warn "EXPOSURE RISK: ${MISSED} suspend(s) occurred without confirmed vault lock"
            warn "  The vault key may have been in S3 RAM during those suspends"
            warn "  Verify: journalctl -t hisnos-vault -g 'VAULT_LOCK' --since=$(cut -d: -f2- "${LOCK_FILE}" 2>/dev/null)"
        else
            info "All ${SUSPEND_EVENTS} suspend(s) had confirmed vault lock events"
        fi
    else
        echo "  suspend_events_since_mount: 0 (no exposure risk)"
    fi
    echo ""

    # ── Forced lazy unmount detection (last 7 days) ───────────────────────────
    LAZY_EVENTS=$(journalctl --user -t "hisnos-vault-watcher" --since=-7days \
        -g "lazily unmounted" --no-pager 2>/dev/null | wc -l || echo 0)
    LOCK_FAIL_EVENTS=$(journalctl --user -t "hisnos-vault-watcher" --since=-7days \
        -g "VAULT_LOCK_FAILED" --no-pager 2>/dev/null | wc -l || echo 0)

    echo "  lazy_unmounts_7d:   ${LAZY_EVENTS}"
    echo "  lock_failures_7d:   ${LOCK_FAIL_EVENTS}"

    if [[ ${LAZY_EVENTS} -gt 0 ]]; then
        warn "Lazy unmounts detected — applications had vault files open during a lock event"
        warn "  Check: journalctl --user -t hisnos-vault-watcher -g 'lazily' --since=-7days"
    fi

    if [[ ${LOCK_FAIL_EVENTS} -gt 0 ]]; then
        warn "Lock failures detected — investigate immediately"
        warn "  Check: journalctl --user -t hisnos-vault-watcher -g 'VAULT_LOCK_FAILED' --since=-7days"
    fi

    # ── Recent vault lock event log (last 10 events) ──────────────────────────
    echo ""
    echo -e "  ${DIM}Recent vault events (last 10):${NC}"
    journalctl -t "hisnos-vault" --no-pager --since=-7days 2>/dev/null \
        | grep -E "VAULT_(LOCKED|LOCK_FAILED)" | tail -10 \
        | sed 's/^/    /' \
        || echo "    (no vault events in journal)"

    echo ""
    echo -e "  ${DIM}Full audit: journalctl -t hisnos-vault --since=-7days${NC}"
}

# ── Command dispatch ──────────────────────────────────────────────────────────
CMD="${1:-status}"

case "${CMD}" in
    init)        cmd_init ;;
    mount)       cmd_mount ;;
    lock)        cmd_lock ;;
    status)      cmd_status ;;
    rotate-key)  cmd_rotate_key ;;
    check)       cmd_check ;;
    telemetry)   cmd_telemetry ;;
    *)
        echo "Usage: ${0} <command>"
        echo ""
        echo "Commands:"
        echo "  init        Initialise a new encrypted vault (one-time setup)"
        echo "  mount       Unlock and mount vault (prompts for passphrase)"
        echo "  lock        Unmount and lock vault (removes key from memory)"
        echo "  status      Show vault state (mounted / locked / uninitialised)"
        echo "  rotate-key  Change vault passphrase"
        echo "  check       Verify vault filesystem integrity (gocryptfs -fsck)"
        echo "  telemetry   Show mounted duration, suspend exposure, lazy unmount events"
        echo ""
        echo "Paths:"
        echo "  Cipher dir: ${CIPHER_DIR}"
        echo "  Mount dir:  ${MOUNT_DIR}"
        echo ""
        echo "Auto-lock triggers (via vault/systemd/ units):"
        echo "  - KDE screen lock (D-Bus: org.freedesktop.ScreenSaver.ActiveChanged)"
        echo "  - Suspend       (D-Bus: org.freedesktop.login1.Manager.PrepareForSleep)"
        echo "  - Idle timeout  (hisnos-vault-idle.timer, default 5 minutes)"
        exit 1
        ;;
esac
