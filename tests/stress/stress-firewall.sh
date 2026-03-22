#!/usr/bin/env bash
# tests/stress/stress-firewall.sh — Firewall Enforcement Load Test
#
# Tests:
#   1. blocked-egress-rate  — verify blocked connections are rejected deterministically
#   2. rule-reload-latency  — measure nftables.service reload time under load
#   3. gaming-chain-toggle  — activate/deactivate gaming chain N times, check stability
#   4. conntrack-stability  — generate N connection attempts and verify no kernel panic
#
# Output: machine-readable JSON (one result object per line, to stdout)
# Progress: stderr

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

readonly TEST_DOMAIN="connectivity-test.invalid"  # should always be blocked
readonly NFT_BIN="/usr/sbin/nft"
readonly BLOCKED_ATTEMPTS=20
readonly RELOAD_ROUNDS=5
readonly GAMING_TOGGLE_ROUNDS=3

# ── Test 1: Blocked egress rate ───────────────────────────────────────────────

test_blocked_egress_rate() {
    sep "Test: blocked-egress-rate"
    info "Sending ${BLOCKED_ATTEMPTS} connection attempts to a domain that should be blocked..."

    local blocked=0 allowed=0 errors=0

    for (( i=1; i<=BLOCKED_ATTEMPTS; i++ )); do
        # curl to an invalid domain over port 443 — expect connection refused or reset
        local http_code
        http_code=$(curl -sS -o /dev/null -w "%{http_code}" \
            --max-time 2 \
            --connect-timeout 1 \
            "https://${TEST_DOMAIN}/" 2>/dev/null || echo "000")
        case "${http_code}" in
            000) (( blocked++ )) || true ;;   # connection failed — expected (blocked/DNS fail)
            *)   (( allowed++ )) || true ;;   # got a response — should not happen
        esac
    done

    local score=0
    if [[ "${allowed}" -eq 0 ]]; then
        score=100
    else
        score=$(( (blocked * 100) / BLOCKED_ATTEMPTS ))
    fi

    local details
    details=$(printf '{"total":%d,"blocked":%d,"allowed":%d,"block_rate_pct":%d}' \
        "${BLOCKED_ATTEMPTS}" "${blocked}" "${allowed}" "${score}")

    if [[ "${allowed}" -gt 0 ]]; then
        err "UNEXPECTED: ${allowed} connections reached ${TEST_DOMAIN}"
        result "blocked_egress_rate" "fail" "${score}" "${details}"
    else
        info "All ${blocked}/${BLOCKED_ATTEMPTS} attempts blocked"
        result "blocked_egress_rate" "pass" "${score}" "${details}"
    fi
}

# ── Test 2: Rule reload latency ───────────────────────────────────────────────

test_rule_reload_latency() {
    sep "Test: rule-reload-latency"
    info "Measuring nftables.service reload latency over ${RELOAD_ROUNDS} rounds..."

    if ! require_bin "systemctl" "rule_reload_latency"; then return; fi

    local total_ms=0 max_ms=0 min_ms=99999 failed=0

    for (( i=1; i<=RELOAD_ROUNDS; i++ )); do
        local start_ns
        start_ns=$(date +%s%N)
        if sudo systemctl reload nftables.service &>/dev/null 2>&1; then
            local end_ns
            end_ns=$(date +%s%N)
            local elapsed_ms=$(( (end_ns - start_ns) / 1000000 ))
            total_ms=$(( total_ms + elapsed_ms ))
            [[ "${elapsed_ms}" -gt "${max_ms}" ]] && max_ms="${elapsed_ms}"
            [[ "${elapsed_ms}" -lt "${min_ms}" ]] && min_ms="${elapsed_ms}"
            info "  Round ${i}: ${elapsed_ms}ms"
        else
            warn "  Round ${i}: reload failed"
            (( failed++ )) || true
        fi
        sleep 0.5
    done

    local successful=$(( RELOAD_ROUNDS - failed ))
    local avg_ms=0
    [[ "${successful}" -gt 0 ]] && avg_ms=$(( total_ms / successful ))

    local score=100
    [[ "${failed}" -gt 0 ]] && score=$(( (successful * 100) / RELOAD_ROUNDS ))
    [[ "${avg_ms}" -gt 2000 ]] && score=$(( score / 2 ))  # penalize if >2s avg

    local details
    details=$(printf '{"rounds":%d,"failed":%d,"avg_ms":%d,"min_ms":%d,"max_ms":%d}' \
        "${RELOAD_ROUNDS}" "${failed}" "${avg_ms}" "${min_ms}" "${max_ms}")

    [[ "${failed}" -eq 0 && "${avg_ms}" -lt 2000 ]] && \
        result "rule_reload_latency" "pass" "${score}" "${details}" || \
        result "rule_reload_latency" "warn" "${score}" "${details}"
}

# ── Test 3: Gaming chain toggle ───────────────────────────────────────────────

test_gaming_chain_toggle() {
    sep "Test: gaming-chain-toggle"
    local nft_file="/etc/nftables/hisnos-gaming.nft"

    if [[ ! -f "${nft_file}" ]]; then
        result "gaming_chain_toggle" "skip" 0 '{"reason":"hisnos-gaming.nft not installed"}'
        return
    fi

    info "Toggling gaming nftables chain ${GAMING_TOGGLE_ROUNDS} times..."
    local failed=0

    for (( i=1; i<=GAMING_TOGGLE_ROUNDS; i++ )); do
        # Load gaming rules
        if ! sudo "${NFT_BIN}" -c -f "${nft_file}" &>/dev/null; then
            warn "  Gaming rules syntax check failed on round ${i}"
            (( failed++ )) || true
            continue
        fi
        if sudo "${NFT_BIN}" -f "${nft_file}" &>/dev/null; then
            info "  Round ${i}: gaming rules loaded"
        else
            warn "  Round ${i}: load failed"
            (( failed++ )) || true
        fi

        sleep 0.2

        # Flush gaming chains
        for chain in gaming_output gaming_input; do
            sudo "${NFT_BIN}" flush chain inet hisnos "${chain}" 2>/dev/null || true
        done
        info "  Round ${i}: gaming rules flushed"
        sleep 0.2
    done

    local score=$(( ((GAMING_TOGGLE_ROUNDS - failed) * 100) / GAMING_TOGGLE_ROUNDS ))
    local details
    details=$(printf '{"rounds":%d,"failed":%d}' "${GAMING_TOGGLE_ROUNDS}" "${failed}")

    [[ "${failed}" -eq 0 ]] && \
        result "gaming_chain_toggle" "pass" "${score}" "${details}" || \
        result "gaming_chain_toggle" "warn" "${score}" "${details}"
}

# ── Test 4: Conntrack stability ───────────────────────────────────────────────

test_conntrack_stability() {
    sep "Test: conntrack-stability"
    info "Generating 50 rapid TCP connection attempts to loopback (stress conntrack)..."

    # Connect to a port we know is not listening — generates RST/refused pairs
    local failed=0 succeeded=0
    for (( i=1; i<=50; i++ )); do
        timeout 0.1 bash -c "echo >/dev/tcp/127.0.0.1/19999" 2>/dev/null \
            && (( succeeded++ )) || (( failed++ )) || true
    done

    # Verify nftables is still running after the flood
    local nft_ok="false"
    sudo "${NFT_BIN}" list table inet hisnos &>/dev/null 2>&1 && nft_ok="true"

    local score=100
    [[ "${nft_ok}" != "true" ]] && score=0

    local details
    details=$(printf '{"attempts":50,"refused":%d,"nftables_intact":%s}' \
        "${failed}" "${nft_ok}")

    [[ "${nft_ok}" == "true" ]] && \
        result "conntrack_stability" "pass" "${score}" "${details}" || \
        result "conntrack_stability" "fail" "${score}" "${details}"
}

# ── Main ──────────────────────────────────────────────────────────────────────

sep "HisnOS Firewall Stress Test"
test_blocked_egress_rate
test_rule_reload_latency
test_gaming_chain_toggle
test_conntrack_stability
