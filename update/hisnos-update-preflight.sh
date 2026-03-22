#!/usr/bin/env bash
# update/hisnos-update-preflight.sh — HisnOS update preflight safety checks
#
# Run before staging (prepare) or applying (apply) a system update.
# Validates that the system is in a safe state to proceed with the update.
#
# Exit codes:
#   0 — all checks passed (or only warnings)
#   1 — one or more checks FAILED (blocking)
#
# Checks:
#   1.  DNS reachability (resolves fedoraproject.org)
#   2.  Network connectivity to Fedora update repositories
#   3.  Firewall enforce mode (nftables table present)
#   4.  Vault mounted state warning (not blocking, but logged)
#   5.  GameMode active check (active gaming session → warn)
#   6.  Disk space headroom (/var at least 3 GB free)
#   7.  Pending staged deployment check (already staged → skip for prepare)
#   8.  System not in degraded state
#
# Usage:
#   hisnos-update-preflight.sh --prepare   # called by hisnos-update prepare
#   hisnos-update-preflight.sh --apply     # called by hisnos-update apply
#   hisnos-update-preflight.sh --dry-run   # print results, no exit 1
#
# Override thresholds via environment variables:
#   PREFLIGHT_MIN_DISK_MB    Minimum /var free space in MiB   (default: 3072)
#   PREFLIGHT_DNS_HOST       Hostname to resolve for DNS check (default: fedoraproject.org)
#   PREFLIGHT_REPO_URL       URL to fetch for network check   (default: see below)

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
LOG_TAG="hisnos-update-preflight"
MODE="${1:---prepare}"
DRY_RUN=false

PREFLIGHT_MIN_DISK_MB="${PREFLIGHT_MIN_DISK_MB:-3072}"   # 3 GiB
PREFLIGHT_DNS_HOST="${PREFLIGHT_DNS_HOST:-fedoraproject.org}"
PREFLIGHT_REPO_URL="${PREFLIGHT_REPO_URL:-https://dl.fedoraproject.org/pub/fedora/linux/}"

XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"

for arg in "$@"; do
    [[ "${arg}" == "--dry-run" ]] && DRY_RUN=true
done

# ── Logging ───────────────────────────────────────────────────────────────────
log()      { echo "[${LOG_TAG}] $*" >&2; }
log_out()  { echo "$*"; }
pass()     { log_out "  [PASS] $*"; }
warn()     { log_out "  [WARN] $*"; log "WARNING: $*"; }
fail()     { log_out "  [FAIL] $*"; log "FAIL: $*"; }

# ── Result tracking ───────────────────────────────────────────────────────────
FAILURES=0
WARNINGS=0

record_fail() { (( FAILURES++ )) || true; fail "$@"; }
record_warn() { (( WARNINGS++ )) || true; warn "$@"; }
record_pass() { pass "$@"; }

# ── Check functions ───────────────────────────────────────────────────────────

check_dns() {
    log_out ""
    log_out "[ DNS Reachability ]"

    if ! command -v getent &>/dev/null && ! command -v host &>/dev/null; then
        record_warn "Neither getent nor host found — skipping DNS check"
        return
    fi

    if getent hosts "${PREFLIGHT_DNS_HOST}" &>/dev/null 2>&1 \
       || host "${PREFLIGHT_DNS_HOST}" &>/dev/null 2>&1; then
        record_pass "${PREFLIGHT_DNS_HOST} resolves"
    else
        record_fail "DNS resolution failed for ${PREFLIGHT_DNS_HOST}"
        log_out "       Verify network connectivity and DNS configuration"
    fi
}

check_network() {
    log_out ""
    log_out "[ Fedora Repository Connectivity ]"

    if ! command -v curl &>/dev/null; then
        record_warn "curl not found — skipping network connectivity check"
        return
    fi

    local http_code
    http_code="$(curl --silent --output /dev/null --write-out "%{http_code}" \
        --max-time 10 --connect-timeout 5 \
        "${PREFLIGHT_REPO_URL}" 2>/dev/null || echo "000")"

    if [[ "${http_code}" =~ ^[23] ]]; then
        record_pass "Fedora repository reachable (HTTP ${http_code})"
    elif [[ "${http_code}" == "000" ]]; then
        record_fail "Cannot reach Fedora repository (connection failed or timed out)"
        log_out "       URL: ${PREFLIGHT_REPO_URL}"
        log_out "       Check: network, firewall CIDR allowlists, OpenSnitch rules"
    else
        record_warn "Fedora repository returned HTTP ${http_code} — may be transient"
        log_out "       URL: ${PREFLIGHT_REPO_URL}"
    fi
}

check_firewall() {
    log_out ""
    log_out "[ Firewall State ]"

    if ! command -v nft &>/dev/null; then
        record_warn "nft not found — cannot verify firewall state"
        return
    fi

    # Check that the hisnos table exists and has rules loaded
    if nft list table inet hisnos_egress &>/dev/null 2>&1; then
        local rule_count
        rule_count="$(nft list table inet hisnos_egress 2>/dev/null \
            | grep -c "^\s*\(accept\|drop\|reject\|queue\)" || echo 0)"
        record_pass "hisnos_egress table loaded (${rule_count} terminal rules)"
    elif nft list ruleset &>/dev/null 2>&1; then
        local total_tables
        total_tables="$(nft list tables 2>/dev/null | wc -l || echo 0)"
        if (( total_tables == 0 )); then
            record_fail "nft ruleset is empty — firewall not loaded"
            log_out "       Start firewall: systemctl start nftables"
        else
            record_warn "hisnos_egress table not found (${total_tables} other tables present)"
            log_out "       HisnOS firewall rules may not be active"
        fi
    else
        record_fail "Cannot access nft ruleset (permission denied or nftables not running)"
    fi
}

check_vault() {
    log_out ""
    log_out "[ Vault State ]"

    local lock_file="${XDG_RUNTIME_DIR}/hisnos-vault.lock"

    if [[ -f "${lock_file}" ]]; then
        local mount_ts
        mount_ts="$(cut -d: -f2- "${lock_file}" 2>/dev/null || echo "unknown")"
        if [[ "${MODE}" == "--apply" ]]; then
            # apply path: vault will be locked automatically — just inform
            record_warn "Vault is mounted (since: ${mount_ts}) — will be locked before reboot"
        else
            # prepare path: vault mount is fine, just log
            record_warn "Vault is mounted (since: ${mount_ts}) — update staging is safe while mounted"
        fi
    else
        record_pass "Vault is locked (no exposure during update)"
    fi
}

check_gamemode() {
    log_out ""
    log_out "[ Gaming Session State ]"

    if ! command -v gamemoded &>/dev/null && ! command -v gamemoderun &>/dev/null; then
        record_pass "GameMode not installed (no gaming session concern)"
        return
    fi

    # Check if GameMode is active (managing any game processes)
    if command -v gamemoded &>/dev/null; then
        local active_clients
        active_clients="$(gamemoded -s 2>/dev/null | grep -c "active client" || echo 0)"
        if (( active_clients > 0 )); then
            record_warn "GameMode active — ${active_clients} client(s) running"
            log_out "       An active gaming session is in progress"
            log_out "       Consider deferring update until gaming session ends"
        else
            record_pass "GameMode: no active gaming sessions"
        fi
    else
        record_pass "GameMode daemon not running"
    fi
}

check_disk_space() {
    log_out ""
    log_out "[ Disk Space (/var) ]"

    local available_mb
    available_mb="$(df --output=avail -m /var 2>/dev/null | tail -1 | tr -d ' ' || echo 0)"

    if (( available_mb >= PREFLIGHT_MIN_DISK_MB )); then
        record_pass "/var: ${available_mb} MiB free (minimum: ${PREFLIGHT_MIN_DISK_MB} MiB)"
    elif (( available_mb >= (PREFLIGHT_MIN_DISK_MB / 2) )); then
        record_warn "/var: only ${available_mb} MiB free (minimum: ${PREFLIGHT_MIN_DISK_MB} MiB)"
        log_out "       Update may succeed but disk space is low"
        log_out "       Consider cleaning: rpm-ostree cleanup -p"
    else
        record_fail "/var: ${available_mb} MiB free — insufficient (minimum: ${PREFLIGHT_MIN_DISK_MB} MiB)"
        log_out "       Free disk space before updating"
        log_out "       Clean old deployments: rpm-ostree cleanup -p"
    fi
}

check_staged_deployment() {
    log_out ""
    log_out "[ Staged Deployment ]"

    if ! command -v rpm-ostree &>/dev/null; then
        record_warn "rpm-ostree not found — cannot check staged deployment"
        return
    fi

    local staged
    staged="$(rpm-ostree status --json 2>/dev/null \
        | python3 -c "
import json,sys
data=json.load(sys.stdin)
for d in data.get('deployments',[]):
    if d.get('staged'):
        print(d.get('checksum','unknown'))
        break
" 2>/dev/null || echo "")"

    if [[ -n "${staged}" ]]; then
        if [[ "${MODE}" == "--prepare" ]]; then
            record_warn "A staged deployment already exists: ${staged}"
            log_out "       Preparing again will replace it with a fresh update"
            log_out "       To apply the existing staged deployment: hisnos-update apply"
        else
            record_pass "Staged deployment ready: ${staged}"
        fi
    else
        if [[ "${MODE}" == "--apply" ]]; then
            record_fail "No staged deployment found — run 'hisnos-update prepare' first"
        else
            record_pass "No conflicting staged deployment"
        fi
    fi
}

check_system_state() {
    log_out ""
    log_out "[ System State ]"

    if ! command -v systemctl &>/dev/null; then
        record_warn "systemctl not found — skipping system state check"
        return
    fi

    local state
    state="$(systemctl is-system-running 2>/dev/null || echo "unknown")"

    case "${state}" in
        running)
            record_pass "System running (all units healthy)"
            ;;
        degraded)
            local failed_units
            failed_units="$(systemctl --state=failed --no-legend list-units 2>/dev/null \
                | awk '{print $1}' | tr '\n' ' ' | sed 's/ $//' || echo "unknown")"
            record_warn "System in degraded state — failed units: ${failed_units}"
            log_out "       Investigate before updating: systemctl --failed"
            ;;
        starting)
            record_warn "System still starting up — may be too early to update"
            ;;
        *)
            record_warn "System state: ${state}"
            ;;
    esac
}

# ── Main ──────────────────────────────────────────────────────────────────────
log_out "=== HisnOS Update Preflight (mode: ${MODE}) ==="

check_dns
check_network
check_firewall
check_vault
check_gamemode
check_disk_space
check_staged_deployment
check_system_state

# ── Summary ───────────────────────────────────────────────────────────────────
log_out ""
log_out "=== Preflight Summary ==="
log_out "  Failures: ${FAILURES}"
log_out "  Warnings: ${WARNINGS}"

logger -t "${LOG_TAG}" "PREFLIGHT mode=${MODE} failures=${FAILURES} warnings=${WARNINGS}" \
    2>/dev/null || true

if (( FAILURES > 0 )); then
    log_out ""
    log_out "Preflight FAILED — ${FAILURES} blocking issue(s) detected."
    log_out "Resolve the issues above before proceeding."
    if [[ "${DRY_RUN}" == "true" ]]; then
        log_out "(dry-run: exit 0 despite failures)"
        exit 0
    fi
    exit 1
else
    if (( WARNINGS > 0 )); then
        log_out ""
        log_out "Preflight passed with ${WARNINGS} warning(s). Proceeding."
    else
        log_out ""
        log_out "Preflight PASSED — system ready for update."
    fi
    exit 0
fi
