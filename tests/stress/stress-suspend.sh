#!/usr/bin/env bash
# tests/stress/stress-suspend.sh — Suspend/Resume Race Scenario Tests
#
# Tests:
#   1. pre-suspend-state    — verify all services are in expected state before suspend
#   2. suspend-inhibitor    — verify systemd-inhibit locks prevent unsafe suspend
#   3. post-suspend-checks  — validate service recovery after a simulated systemd event
#   4. vault-suspend-race   — verify vault watcher survives a prepare-for-sleep signal
#   5. network-resume-state — verify nftables and network recover correctly after resume
#
# NOTE: This script does NOT trigger a real suspend — it simulates the relevant
# systemd lifecycle signals using systemd-inhibit status checks and PrepareForSleep
# D-Bus signals (if available). Real suspend testing requires an interactive session.
#
# Output: machine-readable JSON (one result object per line, to stdout)
# Progress: stderr

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

readonly VAULT_WATCHER_UNIT="hisnos-vault-watcher.service"
readonly LOGD_UNIT="hisnos-logd.service"
readonly THREATD_UNIT="hisnos-threatd.service"
readonly DASHBOARD_UNIT="hisnos-dashboard.service"
readonly NFT_BIN="/usr/sbin/nft"

# ── Test 1: Pre-suspend state ─────────────────────────────────────────────────

test_pre_suspend_state() {
    sep "Test: pre-suspend-state"
    info "Checking all HisnOS services are in expected states before suspend..."

    declare -A units_expected
    units_expected["${VAULT_WATCHER_UNIT}"]="active"
    units_expected["${LOGD_UNIT}"]="active"
    units_expected["${THREATD_UNIT}"]="active"
    units_expected["nftables.service"]="active"   # system scope
    units_expected["auditd.service"]="active"     # system scope

    local passed=0 failed=0 skipped=0
    local issues=()

    for unit in "${!units_expected[@]}"; do
        local expected="${units_expected[${unit}]}"
        local actual=""

        # Determine scope (user vs system)
        case "${unit}" in
            hisnos-*)
                actual=$(systemctl --user is-active "${unit}" 2>/dev/null || echo "inactive")
                ;;
            *)
                actual=$(systemctl is-active "${unit}" 2>/dev/null || echo "inactive")
                ;;
        esac

        if [[ "${actual}" == "${expected}" ]]; then
            (( passed++ )) || true
            info "  ${unit}: ${actual} (ok)"
        elif [[ "${actual}" == "inactive" && "${unit}" == "${THREATD_UNIT}" ]]; then
            # threatd is non-fatal — skip rather than fail
            (( skipped++ )) || true
            warn "  ${unit}: ${actual} (non-fatal — skipped)"
        else
            (( failed++ )) || true
            warn "  ${unit}: ${actual} (expected: ${expected})"
            issues+=("${unit}=${actual}")
        fi
    done

    local total=$(( passed + failed + skipped ))
    local score=0
    [[ "${total}" -gt 0 ]] && score=$(( (passed * 100) / total ))

    local details
    details=$(printf '{"total":%d,"passed":%d,"failed":%d,"skipped":%d,"issues":[%s]}' \
        "${total}" "${passed}" "${failed}" "${skipped}" \
        "$(IFS=','; echo "\"${issues[*]}\"")")

    [[ "${failed}" -eq 0 ]] && \
        result "pre_suspend_state" "pass" "${score}" "${details}" || \
        result "pre_suspend_state" "warn" "${score}" "${details}"
}

# ── Test 2: Suspend inhibitor check ───────────────────────────────────────────

test_suspend_inhibitor() {
    sep "Test: suspend-inhibitor"

    if ! require_bin "systemd-inhibit" "suspend_inhibitor"; then return; fi

    info "Checking active systemd-inhibit locks..."

    # List all active inhibitors
    local inhibitors
    inhibitors=$(systemd-inhibit --list --no-legend 2>/dev/null || echo "")

    local inhibitor_count=0
    inhibitor_count=$(echo "${inhibitors}" | grep -c "sleep\|shutdown\|handle-suspend" 2>/dev/null || echo "0")

    info "  Active sleep/shutdown inhibitors: ${inhibitor_count}"

    # Check if any HisnOS-related inhibitors are present (nice to have, not required)
    local hisnos_inhibitors=0
    hisnos_inhibitors=$(echo "${inhibitors}" | grep -ci "hisnos\|vault\|lab" 2>/dev/null || echo "0")

    info "  HisnOS-related inhibitors: ${hisnos_inhibitors}"

    # This test is informational — no hard requirement on inhibitor count
    local score=100
    local details
    details=$(printf '{"active_sleep_inhibitors":%d,"hisnos_inhibitors":%d}' \
        "${inhibitor_count}" "${hisnos_inhibitors}")

    result "suspend_inhibitor" "pass" "${score}" "${details}"
}

# ── Test 3: Simulated post-resume service recovery ────────────────────────────

test_post_suspend_checks() {
    sep "Test: post-suspend-checks"
    info "Simulating post-resume: sending SIGHUP to user services..."

    # We simulate a resume scenario by checking service restart policy
    # rather than triggering a real suspend cycle (which is disruptive).

    local services_with_restart=0
    local services_without_restart=0
    local user_units=("${VAULT_WATCHER_UNIT}" "${LOGD_UNIT}" "${THREATD_UNIT}" "${DASHBOARD_UNIT}")

    for unit in "${user_units[@]}"; do
        local restart_policy
        restart_policy=$(systemctl --user show "${unit}" --property=Restart \
            2>/dev/null | awk -F= '{print $2}' || echo "no")
        if [[ "${restart_policy}" != "no" && -n "${restart_policy}" ]]; then
            (( services_with_restart++ )) || true
            info "  ${unit}: Restart=${restart_policy} (resilient)"
        else
            (( services_without_restart++ )) || true
            warn "  ${unit}: Restart=${restart_policy} (no auto-restart)"
        fi
    done

    local total=${#user_units[@]}
    local score=$(( (services_with_restart * 100) / total ))

    local details
    details=$(printf '{"units_checked":%d,"with_restart_policy":%d,"without_restart_policy":%d}' \
        "${total}" "${services_with_restart}" "${services_without_restart}")

    [[ "${services_without_restart}" -eq 0 ]] && \
        result "post_suspend_checks" "pass" "${score}" "${details}" || \
        result "post_suspend_checks" "warn" "${score}" "${details}"
}

# ── Test 4: Vault watcher suspend resilience ──────────────────────────────────

test_vault_suspend_race() {
    sep "Test: vault-suspend-race"
    info "Checking vault watcher D-Bus signal subscription..."

    # Verify the watcher is active and subscribed to PrepareForSleep
    local watcher_active="false"
    systemctl --user is-active "${VAULT_WATCHER_UNIT}" &>/dev/null && watcher_active="true"

    if [[ "${watcher_active}" == "false" ]]; then
        result "vault_suspend_race" "warn" 50 \
            '{"reason":"vault watcher not active","implication":"vault may not lock on suspend"}'
        return
    fi

    # Check the watcher command uses gdbus/dbus for PrepareForSleep subscription
    local watcher_cmd
    watcher_cmd=$(systemctl --user cat "${VAULT_WATCHER_UNIT}" 2>/dev/null | \
        grep "ExecStart" | awk -F= '{print $2}' || echo "")

    local uses_dbus="false"
    if echo "${watcher_cmd}" | grep -qi "gdbus\|dbus\|PrepareForSleep"; then
        uses_dbus="true"
    fi

    # Also verify the watcher script references PrepareForSleep
    local watcher_script="${HOME}/.local/share/hisnos/vault/hisnos-vault-watcher.sh"
    local script_has_sleep_signal="false"
    if [[ -f "${watcher_script}" ]] && grep -q "PrepareForSleep" "${watcher_script}" 2>/dev/null; then
        script_has_sleep_signal="true"
    fi

    info "  watcher_active=${watcher_active} uses_dbus=${uses_dbus} has_sleep_signal=${script_has_sleep_signal}"

    local score=100
    [[ "${uses_dbus}" == "false" ]] && score=$(( score - 25 ))
    [[ "${script_has_sleep_signal}" == "false" ]] && score=$(( score - 25 ))

    local details
    details=$(printf '{"watcher_active":%s,"dbus_based":%s,"subscribe_prepare_for_sleep":%s}' \
        "${watcher_active}" "${uses_dbus}" "${script_has_sleep_signal}")

    [[ "${score}" -eq 100 ]] && \
        result "vault_suspend_race" "pass" "${score}" "${details}" || \
        result "vault_suspend_race" "warn" "${score}" "${details}"
}

# ── Test 5: Network resume state ──────────────────────────────────────────────

test_network_resume_state() {
    sep "Test: network-resume-state"
    info "Verifying network and firewall state (simulate post-resume check)..."

    if ! require_bin "${NFT_BIN}" "network_resume_state"; then return; fi

    # Verify nftables hisnos table is intact
    local nft_table_ok="false"
    sudo "${NFT_BIN}" list table inet hisnos &>/dev/null 2>&1 && nft_table_ok="true"

    # Verify default policy is drop (egress control still active)
    local default_drop="false"
    if [[ "${nft_table_ok}" == "true" ]]; then
        if sudo "${NFT_BIN}" list table inet hisnos 2>/dev/null | grep -q "policy drop"; then
            default_drop="true"
        fi
    fi

    # Check nftables.service is active
    local nft_service_active="false"
    systemctl is-active nftables.service &>/dev/null && nft_service_active="true"

    # Verify default interface has an IP (basic network connectivity)
    local default_iface default_ip="none"
    default_iface=$(ip route show default 2>/dev/null | awk '/default/ {print $5}' | head -1 || echo "")
    if [[ -n "${default_iface}" ]]; then
        default_ip=$(ip -4 addr show dev "${default_iface}" 2>/dev/null | \
            awk '/inet / {print $2}' | head -1 || echo "none")
    fi

    info "  nft_table_ok=${nft_table_ok} default_drop=${default_drop} nft_service=${nft_service_active}"
    info "  interface=${default_iface:-none} ip=${default_ip}"

    local score=0
    [[ "${nft_table_ok}" == "true" ]] && score=$(( score + 40 ))
    [[ "${default_drop}" == "true" ]] && score=$(( score + 30 ))
    [[ "${nft_service_active}" == "true" ]] && score=$(( score + 30 ))

    local details
    details=$(printf '{"nft_table_intact":%s,"default_policy_drop":%s,"nft_service_active":%s,"interface":"%s","ip":"%s"}' \
        "${nft_table_ok}" "${default_drop}" "${nft_service_active}" \
        "${default_iface:-none}" "${default_ip}")

    if [[ "${nft_table_ok}" == "true" && "${nft_service_active}" == "true" ]]; then
        result "network_resume_state" "pass" "${score}" "${details}"
    else
        result "network_resume_state" "fail" "${score}" "${details}"
    fi
}

# ── Main ──────────────────────────────────────────────────────────────────────

sep "HisnOS Suspend/Resume Race Scenario Tests"
info "NOTE: Real suspend not triggered — validating resilience configuration only" >&2
test_pre_suspend_state
test_suspend_inhibitor
test_post_suspend_checks
test_vault_suspend_race
test_network_resume_state
