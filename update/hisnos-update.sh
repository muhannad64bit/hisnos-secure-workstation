#!/usr/bin/env bash
# update/hisnos-update.sh — HisnOS Atomic Update & Rollback Engine
#
# Orchestrates rpm-ostree system updates with vault safety coordination,
# preflight validation, rollback guidance, and reboot deferral support.
#
# Commands:
#   check      — check for available updates (no download)
#   prepare    — stage update in background (download, no reboot yet)
#   apply      — lock vault and reboot into staged deployment
#   status     — show all deployments and validation state
#   rollback   — revert to previous deployment with reboot prompt
#   kernel     — show HisnOS kernel override state and available versions
#   validate   — run post-reboot validation and write state file
#
# Design:
#   - rpm-ostree operations use polkit D-Bus daemon (no sudo required)
#   - Updates are always STAGED: disk written, reboot activates
#   - Vault is locked before reboot (apply command)
#   - Preflight checks run before prepare/apply
#   - Validation state written to /var/lib/hisnos/update-state
#   - Dashboard consumes: GET /api/update/status (reads update-state + rpm-ostree status)
#
# Usage:
#   hisnos-update check
#   hisnos-update prepare          # stages update, safe to defer reboot
#   hisnos-update apply            # locks vault, reboots into staged deployment
#   hisnos-update apply --defer    # stages without rebooting
#   hisnos-update status
#   hisnos-update rollback
#   hisnos-update kernel
#   hisnos-update validate         # run after reboot to confirm health

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
HISNOS_DIR="${HISNOS_DIR:-${HOME}/.local/share/hisnos}"
VAULT_SCRIPT="${HISNOS_VAULT_SCRIPT:-${HISNOS_DIR}/vault/hisnos-vault.sh}"
PREFLIGHT_SCRIPT="${HISNOS_PREFLIGHT_SCRIPT:-$(dirname "$(realpath "$0")")/hisnos-update-preflight.sh}"
VALIDATE_SCRIPT="${HISNOS_VALIDATE_SCRIPT:-${HISNOS_DIR}/update/hisnos-validate.sh}"

# State file: records last validate result, staged deployment hash, timestamps
UPDATE_STATE_FILE="/var/lib/hisnos/update-state"
UPDATE_STATE_DIR="$(dirname "${UPDATE_STATE_FILE}")"

# HisnOS kernel RPM override path (set by bootstrap/post-install.sh)
HISNOS_KERNEL_RPM_DIR="${HISNOS_KERNEL_RPM_DIR:-/var/lib/hisnos/kernel-rpms}"

LOG_TAG="hisnos-update"

# ── Logging ───────────────────────────────────────────────────────────────────
log()      { echo "[${LOG_TAG}] $*" >&2; }
log_out()  { echo "$*"; }
log_warn() { echo "[${LOG_TAG}] WARNING: $*" >&2; }
log_err()  { echo "[${LOG_TAG}] ERROR: $*" >&2; }

die() { log_err "$*"; exit 1; }

# ── Dependency checks ─────────────────────────────────────────────────────────
check_deps() {
    local missing=()
    command -v rpm-ostree &>/dev/null || missing+=("rpm-ostree")
    command -v systemctl  &>/dev/null || missing+=("systemctl")
    if (( ${#missing[@]} > 0 )); then
        die "Missing required commands: ${missing[*]}"
    fi
}

# ── State file helpers ─────────────────────────────────────────────────────────
# State file format (key=value, one per line):
#   last_validate_result=ok|fail|unknown
#   last_validate_time=<ISO-8601>
#   last_validate_deployment=<checksum>
#   staged_deployment=<checksum>
#   staged_prepare_time=<ISO-8601>
#   last_apply_time=<ISO-8601>
#   last_rollback_time=<ISO-8601>

state_read() {
    local key="$1"
    if [[ -f "${UPDATE_STATE_FILE}" ]]; then
        grep -E "^${key}=" "${UPDATE_STATE_FILE}" 2>/dev/null | tail -1 | cut -d= -f2- || true
    fi
}

state_write() {
    local key="$1" value="$2"
    # Ensure state directory exists (created by bootstrap or validate --init)
    if [[ ! -d "${UPDATE_STATE_DIR}" ]]; then
        mkdir -p "${UPDATE_STATE_DIR}" 2>/dev/null || {
            log_warn "Cannot create ${UPDATE_STATE_DIR} — state not persisted"
            return 0
        }
    fi
    # Upsert: remove existing key, append new value
    if [[ -f "${UPDATE_STATE_FILE}" ]]; then
        local tmp
        tmp="$(mktemp)"
        grep -v "^${key}=" "${UPDATE_STATE_FILE}" > "${tmp}" 2>/dev/null || true
        echo "${key}=${value}" >> "${tmp}"
        mv "${tmp}" "${UPDATE_STATE_FILE}"
    else
        echo "${key}=${value}" >> "${UPDATE_STATE_FILE}"
    fi
}

# ── rpm-ostree helpers ────────────────────────────────────────────────────────

# Get the booted deployment checksum
booted_checksum() {
    rpm-ostree status --json 2>/dev/null \
        | python3 -c "
import json,sys
data=json.load(sys.stdin)
for d in data.get('deployments',[]):
    if d.get('booted'):
        print(d.get('checksum','unknown'))
        break
" 2>/dev/null || echo "unknown"
}

# Get the staged (pending) deployment checksum, empty string if none
staged_checksum() {
    rpm-ostree status --json 2>/dev/null \
        | python3 -c "
import json,sys
data=json.load(sys.stdin)
for d in data.get('deployments',[]):
    if d.get('staged'):
        print(d.get('checksum','unknown'))
        break
" 2>/dev/null || echo ""
}

# Get human-readable deployment list
deployment_summary() {
    rpm-ostree status 2>/dev/null || echo "(rpm-ostree status unavailable)"
}

# ── Preflight runner ──────────────────────────────────────────────────────────
run_preflight() {
    local mode="${1:-prepare}"   # prepare | apply

    if [[ ! -x "${PREFLIGHT_SCRIPT}" ]]; then
        log_warn "Preflight script not found at ${PREFLIGHT_SCRIPT} — skipping preflight checks"
        return 0
    fi

    log "Running preflight checks (mode: ${mode})..."
    if ! "${PREFLIGHT_SCRIPT}" "--${mode}"; then
        log_err "Preflight checks FAILED — aborting ${mode}"
        log_err "Fix reported issues and retry, or run: hisnos-update preflight --${mode}"
        return 1
    fi
    log "Preflight checks passed"
}

# ── Vault safety ──────────────────────────────────────────────────────────────
vault_lock_for_reboot() {
    log "Locking vault before reboot..."

    if [[ ! -x "${VAULT_SCRIPT}" ]]; then
        # Try PATH lookup
        if command -v hisnos-vault &>/dev/null; then
            VAULT_SCRIPT="hisnos-vault"
        else
            log_warn "Vault script not found — cannot lock vault before reboot"
            log_warn "SECURITY: Ensure vault is manually locked before rebooting"
            return 0
        fi
    fi

    XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
    local lock_file="${XDG_RUNTIME_DIR}/hisnos-vault.lock"

    if [[ ! -f "${lock_file}" ]]; then
        log "Vault already locked — proceeding with reboot"
        return 0
    fi

    if "${VAULT_SCRIPT}" lock; then
        log "Vault locked successfully"
        logger -t "${LOG_TAG}" "VAULT_LOCKED trigger=pre-reboot-update" 2>/dev/null || true
    else
        log_warn "Vault lock failed — SECURITY: manually lock vault before reboot"
        logger -t "${LOG_TAG}" "VAULT_LOCK_FAILED trigger=pre-reboot-update" 2>/dev/null || true
        # Non-fatal: operator may choose to proceed
        return 0
    fi
}

# ── Command: check ────────────────────────────────────────────────────────────
cmd_check() {
    log_out "=== HisnOS Update Check ==="
    log_out ""

    # Check for base system updates
    log "Checking for rpm-ostree base system updates..."
    log_out "[ Base System ]"
    if rpm-ostree upgrade --check 2>&1; then
        : # output already printed by rpm-ostree
    else
        local rc=$?
        if (( rc == 77 )); then
            log_out "  No updates available"
        else
            log_warn "rpm-ostree upgrade --check exited with code ${rc}"
        fi
    fi

    log_out ""

    # Check for staged deployment
    local staged
    staged="$(staged_checksum)"
    if [[ -n "${staged}" ]]; then
        log_out "[ Staged Deployment ]"
        log_out "  A staged update is ready — reboot to apply"
        log_out "  Checksum: ${staged}"
        log_out ""
        log_out "  To apply:  hisnos-update apply"
        log_out "  To defer:  reboot at your convenience (staged persists)"
        log_out ""
    fi

    # Check HisnOS kernel override state
    log_out "[ HisnOS Kernel ]"
    cmd_kernel --brief
    log_out ""

    # Last validation state
    log_out "[ Last Validation ]"
    local last_result last_time last_deploy
    last_result="$(state_read last_validate_result)"
    last_time="$(state_read last_validate_time)"
    last_deploy="$(state_read last_validate_deployment)"

    if [[ -z "${last_result}" ]]; then
        log_out "  No validation record found"
        log_out "  Run: hisnos-update validate   (after each reboot)"
    else
        log_out "  Result:     ${last_result}"
        log_out "  Time:       ${last_time:-unknown}"
        log_out "  Deployment: ${last_deploy:-unknown}"
    fi
}

# ── Command: prepare ──────────────────────────────────────────────────────────
cmd_prepare() {
    log_out "=== HisnOS Update Prepare ==="

    run_preflight "prepare" || return 1

    log_out ""
    log_out "Staging rpm-ostree upgrade (downloading in background)..."
    log_out "This may take several minutes depending on update size."
    log_out ""

    if rpm-ostree upgrade --allow-downgrade 2>&1; then
        local staged
        staged="$(staged_checksum)"
        log_out ""
        log_out "Update staged successfully."
        if [[ -n "${staged}" ]]; then
            log_out "  Staged checksum: ${staged}"
            state_write "staged_deployment" "${staged}"
            state_write "staged_prepare_time" "$(date --iso-8601=seconds)"
        fi
        log_out ""
        log_out "  Reboot when ready:  hisnos-update apply"
        log_out "  Defer reboot:       use the workstation normally; staged update persists"
        log_out "  Check status:       hisnos-update status"
        logger -t "${LOG_TAG}" "UPDATE_STAGED checksum=${staged:-unknown}" 2>/dev/null || true
    else
        local rc=$?
        if (( rc == 77 )); then
            log_out "No updates available — system is already up to date"
        else
            die "rpm-ostree upgrade failed (exit code ${rc})"
        fi
    fi
}

# ── Command: apply ────────────────────────────────────────────────────────────
cmd_apply() {
    local defer=false
    for arg in "$@"; do
        [[ "${arg}" == "--defer" ]] && defer=true
    done

    log_out "=== HisnOS Update Apply ==="

    # Check that a staged deployment actually exists
    local staged
    staged="$(staged_checksum)"
    if [[ -z "${staged}" ]]; then
        log_out ""
        log_out "No staged deployment found."
        log_out "Run 'hisnos-update prepare' first to stage an update."
        log_out ""
        log_out "If you are trying to apply an already-downloaded update,"
        log_out "check rpm-ostree status for deployment details."
        return 1
    fi

    log_out ""
    log_out "Staged deployment: ${staged}"

    run_preflight "apply" || return 1

    # Lock vault before reboot
    vault_lock_for_reboot

    state_write "last_apply_time" "$(date --iso-8601=seconds)"
    logger -t "${LOG_TAG}" "UPDATE_APPLY_INITIATED checksum=${staged}" 2>/dev/null || true

    if [[ "${defer}" == "true" ]]; then
        log_out ""
        log_out "Update staged. Reboot deferred by --defer flag."
        log_out "Vault has been locked. Reboot when ready:"
        log_out "  systemctl reboot"
        log_out "  (or use the dashboard Reboot button)"
        return 0
    fi

    log_out ""
    log_out "Rebooting into staged deployment in 5 seconds..."
    log_out "(Ctrl-C to cancel)"
    log_out ""
    sleep 5

    log "Issuing systemctl reboot"
    systemctl reboot
}

# ── Command: status ───────────────────────────────────────────────────────────
cmd_status() {
    log_out "=== HisnOS Update Status ==="
    log_out ""

    # rpm-ostree deployment list
    log_out "[ Deployments ]"
    deployment_summary
    log_out ""

    # Staged state
    local staged booted
    staged="$(staged_checksum)"
    booted="$(booted_checksum)"

    log_out "[ Deployment State ]"
    log_out "  Booted:  ${booted}"
    if [[ -n "${staged}" ]]; then
        log_out "  Staged:  ${staged}  ← pending reboot"
    else
        log_out "  Staged:  (none)"
    fi

    # State file values
    local staged_time apply_time rollback_time
    staged_time="$(state_read staged_prepare_time)"
    apply_time="$(state_read last_apply_time)"
    rollback_time="$(state_read last_rollback_time)"

    [[ -n "${staged_time}"   ]] && log_out "  Staged at:    ${staged_time}"
    [[ -n "${apply_time}"    ]] && log_out "  Apply issued: ${apply_time}"
    [[ -n "${rollback_time}" ]] && log_out "  Last rollback: ${rollback_time}"

    log_out ""

    # Validation
    log_out "[ Validation State ]"
    local last_result last_time last_deploy
    last_result="$(state_read last_validate_result)"
    last_time="$(state_read last_validate_time)"
    last_deploy="$(state_read last_validate_deployment)"

    if [[ -z "${last_result}" ]]; then
        log_out "  No validation record"
    else
        log_out "  Result:     ${last_result}"
        log_out "  Time:       ${last_time:-unknown}"
        log_out "  Deployment: ${last_deploy:-unknown}"
        if [[ "${last_deploy}" != "${booted}" ]]; then
            log_out "  NOTE: Validation was for a different deployment than currently booted"
            log_out "        Run: hisnos-update validate"
        fi
    fi

    log_out ""

    # Vault state
    log_out "[ Vault State ]"
    XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
    local lock_file="${XDG_RUNTIME_DIR}/hisnos-vault.lock"
    if [[ -f "${lock_file}" ]]; then
        local mount_ts
        mount_ts="$(cut -d: -f2- "${lock_file}" 2>/dev/null || echo "unknown")"
        log_out "  MOUNTED (since: ${mount_ts})"
    else
        log_out "  LOCKED"
    fi
}

# ── Command: rollback ─────────────────────────────────────────────────────────
cmd_rollback() {
    log_out "=== HisnOS Rollback ==="
    log_out ""

    # Show current deployments first
    log_out "Current deployments:"
    deployment_summary
    log_out ""

    # Check rollback is possible (need ≥2 deployments)
    local deployment_count
    deployment_count="$(rpm-ostree status --json 2>/dev/null \
        | python3 -c "
import json,sys
data=json.load(sys.stdin)
print(len(data.get('deployments',[])))
" 2>/dev/null || echo 0)"

    if (( deployment_count < 2 )); then
        log_out "Only one deployment available — cannot rollback."
        log_out "A previous deployment is required for rollback."
        log_out ""
        log_out "Recovery options if system is broken:"
        log_out "  1. Boot from Fedora Kinoite live USB"
        log_out "  2. Use rpm-ostree from live environment to deploy a known-good ref"
        log_out "  3. See: https://docs.fedoraproject.org/en-US/fedora-kinoite/getting-started/"
        return 1
    fi

    log_out "Rolling back to previous deployment."
    log_out ""

    # Lock vault before rollback reboot
    vault_lock_for_reboot

    # Mark rollback in state
    state_write "last_rollback_time" "$(date --iso-8601=seconds)"
    logger -t "${LOG_TAG}" "ROLLBACK_INITIATED booted=${booted_checksum:-unknown}" 2>/dev/null || true

    # Stage rollback
    if ! rpm-ostree rollback 2>&1; then
        die "rpm-ostree rollback failed"
    fi

    log_out ""
    log_out "Rollback staged."
    log_out ""
    log_out "Reboot to activate the previous deployment:"
    log_out "  systemctl reboot"
    log_out ""
    log_out "After reboot, run validation to confirm system health:"
    log_out "  hisnos-update validate"
    log_out ""
    log_out "NOTE: The rolled-back kernel will be active. If the HisnOS kernel override"
    log_out "caused the issue, check: hisnos-update kernel"
}

# ── Command: kernel ───────────────────────────────────────────────────────────
cmd_kernel() {
    local brief=false
    for arg in "$@"; do
        [[ "${arg}" == "--brief" ]] && brief=true
    done

    if [[ "${brief}" == "false" ]]; then
        log_out "=== HisnOS Kernel Override State ==="
        log_out ""
    fi

    # Check for active kernel override via rpm-ostree
    local override_output
    override_output="$(rpm-ostree status --json 2>/dev/null \
        | python3 -c "
import json,sys
data=json.load(sys.stdin)
for d in data.get('deployments',[]):
    if d.get('booted'):
        overrides = d.get('base-local-replacements',[])
        for o in overrides:
            if 'kernel' in o.get('name','').lower():
                print('active:', o.get('name','?'), o.get('evr','?'))
        if not overrides:
            print('none')
        break
" 2>/dev/null || echo "unknown")"

    if [[ "${override_output}" == "none" || -z "${override_output}" ]]; then
        log_out "  Kernel override: not active (using base Fedora kernel)"
    elif [[ "${override_output}" == "unknown" ]]; then
        log_out "  Kernel override: (status unavailable)"
    else
        log_out "  Kernel override: ${override_output}"
    fi

    if [[ "${brief}" == "true" ]]; then
        return 0
    fi

    log_out ""

    # Show available kernel RPMs in override directory
    if [[ -d "${HISNOS_KERNEL_RPM_DIR}" ]]; then
        local rpms=()
        mapfile -t rpms < <(find "${HISNOS_KERNEL_RPM_DIR}" -name "*.rpm" 2>/dev/null | sort)
        if (( ${#rpms[@]} > 0 )); then
            log_out "  Available kernel RPMs (${HISNOS_KERNEL_RPM_DIR}):"
            for rpm_file in "${rpms[@]}"; do
                log_out "    $(basename "${rpm_file}")"
            done
        else
            log_out "  No kernel RPMs in ${HISNOS_KERNEL_RPM_DIR}"
        fi
    else
        log_out "  Kernel RPM directory not found: ${HISNOS_KERNEL_RPM_DIR}"
        log_out "  (Set HISNOS_KERNEL_RPM_DIR or run bootstrap/post-install.sh)"
    fi

    log_out ""
    log_out "To apply a new kernel override:"
    log_out "  rpm-ostree override replace <kernel-core.rpm> <kernel-modules.rpm> ..."
    log_out ""
    log_out "To remove kernel override (revert to base Fedora kernel):"
    log_out "  rpm-ostree override reset kernel"
    log_out "  systemctl reboot"
}

# ── Command: validate ────────────────────────────────────────────────────────
cmd_validate() {
    log_out "=== HisnOS Post-Reboot Validation ==="
    log_out ""

    local booted
    booted="$(booted_checksum)"
    local now
    now="$(date --iso-8601=seconds)"
    local result="ok"
    local failures=()

    # 1. Check that the booted deployment matches what was staged
    local expected_staged
    expected_staged="$(state_read staged_deployment)"
    if [[ -n "${expected_staged}" && "${booted}" != "${expected_staged}" ]]; then
        log_warn "Booted deployment (${booted}) does not match staged (${expected_staged})"
        log_warn "This may indicate an unintended rollback occurred"
        failures+=("deployment-mismatch")
    fi

    # 2. Run external validation script if available
    if [[ -x "${VALIDATE_SCRIPT}" ]]; then
        log "Running external validate script: ${VALIDATE_SCRIPT}"
        if ! "${VALIDATE_SCRIPT}"; then
            log_warn "External validation script reported failures"
            failures+=("external-validate-failed")
        fi
    else
        log "No external validate script at ${VALIDATE_SCRIPT} — running built-in checks"

        # Built-in check 1: firewall enforcement
        if command -v nft &>/dev/null; then
            if ! nft list ruleset &>/dev/null; then
                log_warn "nft list ruleset failed — firewall may not be loaded"
                failures+=("nft-unavailable")
            else
                local rule_count
                rule_count="$(nft list ruleset 2>/dev/null | grep -c "^\s*\(accept\|drop\|reject\|queue\|goto\|jump\|return\|log\)" || true)"
                if (( rule_count == 0 )); then
                    log_warn "nft ruleset appears empty — firewall may not be active"
                    failures+=("nft-empty")
                else
                    log_out "  Firewall: OK (${rule_count} terminal rules)"
                fi
            fi
        else
            log_out "  Firewall: nft not found (skipped)"
        fi

        # Built-in check 2: user systemd services
        local failed_services
        failed_services="$(systemctl --user --state=failed --no-legend list-units 2>/dev/null | awk '{print $1}' | grep "hisnos" || true)"
        if [[ -n "${failed_services}" ]]; then
            log_warn "Failed HisnOS user services: ${failed_services}"
            failures+=("user-service-failed")
        else
            log_out "  HisnOS user services: OK (no failed units)"
        fi

        # Built-in check 3: kernel boot success (systemd boot was complete)
        if ! systemctl is-system-running --quiet 2>/dev/null; then
            local system_state
            system_state="$(systemctl is-system-running 2>/dev/null || echo "unknown")"
            if [[ "${system_state}" == "degraded" ]]; then
                log_warn "System in degraded state — some units failed"
                failures+=("system-degraded")
            fi
        else
            log_out "  System state: OK (running)"
        fi
    fi

    # Write validation result
    if (( ${#failures[@]} > 0 )); then
        result="fail"
        log_out ""
        log_out "Validation FAILED. Issues detected:"
        for f in "${failures[@]}"; do
            log_out "  - ${f}"
        done
        log_out ""
        log_out "Rollback options:"
        log_out "  hisnos-update rollback    # stage previous deployment"
        log_out "  systemctl reboot          # reboot into rollback after staging"
    else
        log_out ""
        log_out "Validation PASSED. System appears healthy."
    fi

    state_write "last_validate_result"     "${result}"
    state_write "last_validate_time"       "${now}"
    state_write "last_validate_deployment" "${booted}"
    logger -t "${LOG_TAG}" "VALIDATE_RESULT result=${result} deployment=${booted}" 2>/dev/null || true

    log_out ""
    log_out "Result written to: ${UPDATE_STATE_FILE}"

    [[ "${result}" == "ok" ]]
}

# ── Command dispatch ──────────────────────────────────────────────────────────
CMD="${1:-help}"
shift || true

check_deps

case "${CMD}" in
    check)    cmd_check    "$@" ;;
    prepare)  cmd_prepare  "$@" ;;
    apply)    cmd_apply    "$@" ;;
    status)   cmd_status   "$@" ;;
    rollback) cmd_rollback "$@" ;;
    kernel)   cmd_kernel   "$@" ;;
    validate) cmd_validate "$@" ;;
    help|--help|-h)
        cat <<EOF
Usage: hisnos-update <command> [options]

Commands:
  check      Check for available system updates (no download)
  prepare    Stage update download (no reboot required yet)
  apply      Lock vault and reboot into staged deployment
             --defer    Stage without rebooting
  status     Show all deployments, validation state, vault state
  rollback   Revert to previous deployment with reboot prompt
  kernel     Show HisnOS kernel override state and available RPMs
  validate   Run post-reboot health checks and write state file

Environment:
  HISNOS_DIR             Base install dir  (default: ~/.local/share/hisnos)
  HISNOS_VAULT_SCRIPT    Path to hisnos-vault.sh
  HISNOS_PREFLIGHT_SCRIPT  Path to hisnos-update-preflight.sh
  HISNOS_VALIDATE_SCRIPT   Path to hisnos-validate.sh (optional external validator)
  HISNOS_KERNEL_RPM_DIR  Directory containing HisnOS kernel RPMs

State file: ${UPDATE_STATE_FILE}

Examples:
  hisnos-update check
  hisnos-update prepare && hisnos-update apply
  hisnos-update apply --defer   # vault locked, reboot at your convenience
  hisnos-update rollback        # stage rollback, then: systemctl reboot
  hisnos-update validate        # run after every reboot
EOF
        ;;
    *)
        log_err "Unknown command: ${CMD}"
        log_err "Run: hisnos-update help"
        exit 1
        ;;
esac
