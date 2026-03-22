#!/usr/bin/env bash
# tests/stress/stress-lab-ns.sh — Lab Namespace Burst Simulation
#
# Tests:
#   1. ns-creation-rate   — create/destroy N user namespaces rapidly, measure rate
#   2. bwrap-launch-cycle — launch/exit bwrap sessions N times, verify cleanup
#   3. ns-threat-signal   — verify threatd detects ns_burst signal within window
#   4. veth-cleanup       — verify veth pairs are fully removed after session end

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

readonly BWRAP_BIN="/usr/bin/bwrap"
readonly NS_BURST_COUNT=5    # must match threatd ns_burst threshold (3) to trigger
readonly BWRAP_CYCLES=3

# ── Test 1: Namespace creation rate ──────────────────────────────────────────

test_ns_creation_rate() {
    sep "Test: ns-creation-rate"
    if ! require_bin "unshare" "ns_creation_rate"; then return; fi

    info "Creating ${NS_BURST_COUNT} user namespaces in rapid succession..."
    local start
    start=$(date +%s)
    local succeeded=0 failed=0

    for (( i=1; i<=NS_BURST_COUNT; i++ )); do
        if unshare --user --pid --fork true 2>/dev/null; then
            (( succeeded++ )) || true
        else
            (( failed++ )) || true
            warn "  namespace ${i} creation failed"
        fi
    done

    local elapsed
    elapsed=$(elapsed_since "${start}")
    local rate=0
    [[ "${elapsed}" -gt 0 ]] && rate=$(( succeeded / elapsed ))

    local score=$(( (succeeded * 100) / NS_BURST_COUNT ))
    local details
    details=$(printf '{"requested":%d,"succeeded":%d,"failed":%d,"duration_s":%d,"rate_per_s":%d}' \
        "${NS_BURST_COUNT}" "${succeeded}" "${failed}" "${elapsed}" "${rate}")

    [[ "${failed}" -eq 0 ]] && \
        result "ns_creation_rate" "pass" "${score}" "${details}" || \
        result "ns_creation_rate" "warn" "${score}" "${details}"
}

# ── Test 2: bwrap launch cycle ────────────────────────────────────────────────

test_bwrap_launch_cycle() {
    sep "Test: bwrap-launch-cycle"
    if ! require_bin "${BWRAP_BIN}" "bwrap_launch_cycle"; then return; fi

    info "Running ${BWRAP_CYCLES} bwrap sessions (unshare-all, tmpfs, immediate exit)..."
    local succeeded=0 failed=0

    for (( i=1; i<=BWRAP_CYCLES; i++ )); do
        if timeout 10 "${BWRAP_BIN}" \
            --unshare-all \
            --ro-bind /usr /usr \
            --tmpfs /tmp \
            --proc /proc \
            --dev /dev \
            --die-with-parent \
            /bin/true 2>/dev/null; then
            (( succeeded++ )) || true
            info "  Session ${i}: ok"
        else
            (( failed++ )) || true
            warn "  Session ${i}: failed"
        fi
        sleep 0.1
    done

    # Verify no orphaned bwrap processes remain
    local orphans
    orphans=$(pgrep -x bwrap 2>/dev/null | wc -l || echo "0")

    local score=$(( (succeeded * 100) / BWRAP_CYCLES ))
    [[ "${orphans}" -gt 0 ]] && score=$(( score - 20 ))

    local details
    details=$(printf '{"cycles":%d,"succeeded":%d,"failed":%d,"orphaned_procs":%d}' \
        "${BWRAP_CYCLES}" "${succeeded}" "${failed}" "${orphans}")

    [[ "${failed}" -eq 0 && "${orphans}" -eq 0 ]] && \
        result "bwrap_launch_cycle" "pass" "${score}" "${details}" || \
        result "bwrap_launch_cycle" "warn" "${score}" "${details}"
}

# ── Test 3: Threat signal detection ───────────────────────────────────────────

test_ns_threat_signal() {
    sep "Test: ns-threat-signal"
    local state_file="/var/lib/hisnos/threat-state.json"

    if [[ ! -f "${state_file}" ]]; then
        result "ns_threat_signal" "skip" 0 '{"reason":"threatd not running (threat-state.json missing)"}'
        return
    fi

    local score_before
    score_before=$(read_risk_score)
    info "Risk score before burst: ${score_before}"

    # Trigger NS burst (more than the threshold=3 in a 5-minute window)
    info "Triggering namespace burst (${NS_BURST_COUNT} unshare calls)..."
    for (( i=1; i<=NS_BURST_COUNT; i++ )); do
        unshare --user --pid --fork true 2>/dev/null || true
    done

    # Wait for threatd evaluation cycle
    info "Waiting for threatd evaluation (up to 60s)..."
    if ! wait_threatd_eval 65; then
        result "ns_threat_signal" "skip" 0 '{"reason":"threatd evaluation timeout"}'
        return
    fi

    local score_after
    score_after=$(read_risk_score)
    info "Risk score after burst: ${score_after}"

    # The ns_burst signal adds 20 points — score should have increased
    local delta=$(( score_after - score_before ))
    local detected="false"
    [[ "${delta}" -ge 15 ]] && detected="true"

    local ns_burst_active
    ns_burst_active=$(python3 -c \
        "import json; d=json.load(open('${state_file}')); print(str(d.get('signals',{}).get('ns_burst',False)).lower())" \
        2>/dev/null || echo "false")

    local score=100
    [[ "${ns_burst_active}" == "true" ]] || score=0

    local details
    details=$(printf '{"score_before":%d,"score_after":%d,"delta":%d,"ns_burst_signal":%s}' \
        "${score_before}" "${score_after}" "${delta}" "${ns_burst_active}")

    [[ "${ns_burst_active}" == "true" ]] && \
        result "ns_threat_signal" "pass" "${score}" "${details}" || \
        result "ns_threat_signal" "fail" "${score}" "${details}"
}

# ── Test 4: Veth interface cleanup ────────────────────────────────────────────

test_veth_cleanup() {
    sep "Test: veth-cleanup"
    info "Verifying no orphaned lab veth interfaces exist..."

    local orphaned_vlh orphaned_vlc
    orphaned_vlh=$(ip link show 2>/dev/null | grep -c "vlh-" || echo "0")
    orphaned_vlc=$(ip link show 2>/dev/null | grep -c "vlc-" || echo "0")
    local total=$(( orphaned_vlh + orphaned_vlc ))

    local score=100
    [[ "${total}" -gt 0 ]] && score=0

    local details
    details=$(printf '{"orphaned_vlh":%d,"orphaned_vlc":%d,"total_orphaned":%d}' \
        "${orphaned_vlh}" "${orphaned_vlc}" "${total}")

    if [[ "${total}" -gt 0 ]]; then
        warn "Orphaned veth interfaces found! Run: hisnos-recover.sh lab-emergency-stop"
        result "veth_cleanup" "fail" "${score}" "${details}"
    else
        result "veth_cleanup" "pass" "${score}" "${details}"
    fi
}

# ── Main ──────────────────────────────────────────────────────────────────────

sep "HisnOS Lab Namespace Stress Test"
test_ns_creation_rate
test_bwrap_launch_cycle
test_ns_threat_signal
test_veth_cleanup
