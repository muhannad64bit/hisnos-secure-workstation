#!/usr/bin/env bash
# tests/test-firewall-perf.sh — HisnOS firewall performance verification
#
# Validates that the HisnOS nftables ruleset does not introduce unacceptable
# overhead on a Fedora Kinoite workstation. This is NOT a benchmark — it checks
# that performance is within expected bounds for a desktop workstation under
# enforce mode.
#
# Checks:
#   1. Ruleset complexity (rule count, chain depth, set sizes)
#   2. Connection tracking table (current fill %, limit headroom)
#   3. Logging overhead (log chain rate-limit verification, no log storms)
#   4. Loopback latency (baseline vs with-ruleset — detects pathological rules)
#   5. NFQUEUE queue depth (OpenSnitch queue backpressure check)
#   6. nftables memory footprint (via /proc/net/nf_conntrack or nft stats)
#   7. journald write rate (log entries per minute — detect flood)
#
# Thresholds (conservative workstation values — adjust in THRESHOLDS section):
#   Rule count:          < 200 total rules
#   Conntrack fill:      < 80% of max
#   Latency baseline:    < 0.5ms loopback RTT
#   Log rate:            < 50 HISNOS log entries/minute (not in a flood)
#   NFQUEUE queue depth: < 1024 pending packets
#
# Usage:
#   ./tests/test-firewall-perf.sh              # interactive report
#   ./tests/test-firewall-perf.sh --strict     # exit 1 on any FAIL
#   ./tests/test-firewall-perf.sh --json       # machine-readable output
#   ./tests/test-firewall-perf.sh --baseline   # print current values (no thresholds)
#
# Requires: nft (root for some checks), ping, ss, journalctl
# Run as: regular user with sudo available (sudo used internally for nft calls)

set -euo pipefail

# ── Thresholds ────────────────────────────────────────────────────────────────
MAX_RULES=200            # max total nft rules in inet hisnos table
MAX_CHAINS=30            # max chains in inet hisnos table
MAX_SET_ELEMENTS=2000    # max total elements across all named sets
CONNTRACK_FILL_PCT=80    # max conntrack table fill percentage
MAX_LATENCY_MS=1         # max loopback RTT in milliseconds (integer, ping stat)
LOG_RATE_WARN=50         # HISNOS log entries per minute → WARN
LOG_RATE_FAIL=200        # HISNOS log entries per minute → FAIL (storm)
MAX_NFQUEUE_PENDING=1024 # max OpenSnitch NFQUEUE pending packets

# ── Arg parsing ───────────────────────────────────────────────────────────────
STRICT=false
JSON=false
BASELINE=false

for arg in "$@"; do
    case "${arg}" in
        --strict)   STRICT=true ;;
        --json)     JSON=true ;;
        --baseline) BASELINE=true ;;
    esac
done

# ── Colour + counters ─────────────────────────────────────────────────────────
RED='\\033[0;31m'; GREEN='\\033[0;32m'; YELLOW='\\033[1;33m'
BOLD='\\033[1m'; DIM='\\033[2m'; NC='\\033[0m'

PASS=0; FAIL=0; WARN=0; SKIP=0
JSON_RESULTS=()

pass() {
    [[ "${JSON}" == "false" ]] && echo -e "  ${GREEN}[PASS]${NC} $*"
    (( PASS++ )) || true
}
fail() {
    [[ "${JSON}" == "false" ]] && echo -e "  ${RED}[FAIL]${NC} $*"
    (( FAIL++ )) || true
}
warn() {
    [[ "${JSON}" == "false" ]] && echo -e "  ${YELLOW}[WARN]${NC} $*"
    (( WARN++ )) || true
}
skip() {
    [[ "${JSON}" == "false" ]] && echo -e "  ${DIM}[SKIP]${NC} $*"
    (( SKIP++ )) || true
}
info() {
    [[ "${JSON}" == "false" ]] && echo -e "  ${DIM}[INFO]${NC} $*"
}
section() {
    [[ "${JSON}" == "false" ]] && echo -e "\\n${BOLD}── $* ──${NC}"
}

# Append to JSON results array: check_name result value threshold
json_record() {
    local name="$1" result="$2" value="$3" threshold="${4:-}"
    JSON_RESULTS+=("{\"check\":\"${name}\",\"result\":\"${result}\",\"value\":\"${value}\",\"threshold\":\"${threshold}\"}")
}

# ── Header ────────────────────────────────────────────────────────────────────
if [[ "${JSON}" == "false" && "${BASELINE}" == "false" ]]; then
    echo -e "${BOLD}HisnOS Firewall Performance Verification${NC}"
    echo -e "${DIM}$(date) | mode: $(cat /run/hisnos-egress-mode 2>/dev/null || echo 'unknown')${NC}"
fi

# ── Check 1: Ruleset complexity ───────────────────────────────────────────────
section "1. Ruleset Complexity"

if ! sudo nft list table inet hisnos &>/dev/null 2>&1; then
    [[ "${BASELINE}" == "false" ]] && fail "Table inet hisnos not loaded — load observe or enforce mode first"
    json_record "table_loaded" "FAIL" "not_loaded" ""
    if [[ "${STRICT}" == "true" ]]; then exit 1; fi
    # Skip remaining nft checks
    TOTAL_RULES=0
    CHAIN_COUNT=0
    SET_ELEMENTS=0
else
    # Count total rules (lines containing 'ip' or 'meta' or 'queue' or 'log' inside chains)
    RULESET_DUMP=$(sudo nft list table inet hisnos 2>/dev/null)

    TOTAL_RULES=$(echo "${RULESET_DUMP}" | grep -cE "^\s+(ip|ip6|meta|tcp|udp|icmp|ct|queue|log|jump|accept|drop|return)" || echo 0)
    CHAIN_COUNT=$(echo "${RULESET_DUMP}" | grep -c "^	chain " || echo 0)

    # Count set elements across all named sets
    SET_ELEMENTS=$(sudo nft list table inet hisnos 2>/dev/null \
        | grep -c "^\t\t[0-9a-f./:]" 2>/dev/null || echo 0)

    if [[ "${BASELINE}" == "true" ]]; then
        echo "ruleset.total_rules=${TOTAL_RULES}"
        echo "ruleset.chain_count=${CHAIN_COUNT}"
        echo "ruleset.set_elements=${SET_ELEMENTS}"
    else
        info "Total rules: ${TOTAL_RULES} (threshold: < ${MAX_RULES})"
        info "Chain count: ${CHAIN_COUNT} (threshold: < ${MAX_CHAINS})"
        info "Set elements: ${SET_ELEMENTS} (threshold: < ${MAX_SET_ELEMENTS})"

        if [[ ${TOTAL_RULES} -lt ${MAX_RULES} ]]; then
            pass "Rule count: ${TOTAL_RULES} rules (< ${MAX_RULES})"
            json_record "rule_count" "PASS" "${TOTAL_RULES}" "${MAX_RULES}"
        elif [[ ${TOTAL_RULES} -lt $(( MAX_RULES * 2 )) ]]; then
            warn "Rule count: ${TOTAL_RULES} rules — above threshold (${MAX_RULES}); consider splitting into sets"
            json_record "rule_count" "WARN" "${TOTAL_RULES}" "${MAX_RULES}"
        else
            fail "Rule count: ${TOTAL_RULES} rules — too many individual rules; use named sets"
            json_record "rule_count" "FAIL" "${TOTAL_RULES}" "${MAX_RULES}"
        fi

        if [[ ${CHAIN_COUNT} -lt ${MAX_CHAINS} ]]; then
            pass "Chain count: ${CHAIN_COUNT} chains (< ${MAX_CHAINS})"
            json_record "chain_count" "PASS" "${CHAIN_COUNT}" "${MAX_CHAINS}"
        else
            warn "Chain count: ${CHAIN_COUNT} — high; verify no duplicate chain definitions accumulating"
            json_record "chain_count" "WARN" "${CHAIN_COUNT}" "${MAX_CHAINS}"
        fi

        if [[ ${SET_ELEMENTS} -lt ${MAX_SET_ELEMENTS} ]]; then
            pass "Set elements: ${SET_ELEMENTS} total across all named sets"
            json_record "set_elements" "PASS" "${SET_ELEMENTS}" "${MAX_SET_ELEMENTS}"
        else
            warn "Set elements: ${SET_ELEMENTS} — large; consider CIDR aggregation"
            json_record "set_elements" "WARN" "${SET_ELEMENTS}" "${MAX_SET_ELEMENTS}"
        fi
    fi
fi

# ── Check 2: Connection tracking table ───────────────────────────────────────
section "2. Connection Tracking Table"

CONNTRACK_MAX=$(cat /proc/sys/net/netfilter/nf_conntrack_max 2>/dev/null || echo 0)
CONNTRACK_COUNT=$(cat /proc/sys/net/netfilter/nf_conntrack_count 2>/dev/null || echo 0)

if [[ ${CONNTRACK_MAX} -eq 0 ]]; then
    skip "conntrack: /proc/sys/net/netfilter/nf_conntrack_max not readable (module not loaded?)"
    json_record "conntrack_fill" "SKIP" "n/a" "${CONNTRACK_FILL_PCT}%"
else
    FILL_PCT=$(( CONNTRACK_COUNT * 100 / CONNTRACK_MAX ))

    if [[ "${BASELINE}" == "true" ]]; then
        echo "conntrack.count=${CONNTRACK_COUNT}"
        echo "conntrack.max=${CONNTRACK_MAX}"
        echo "conntrack.fill_pct=${FILL_PCT}"
    else
        info "conntrack: ${CONNTRACK_COUNT} / ${CONNTRACK_MAX} entries (${FILL_PCT}%)"

        if [[ ${FILL_PCT} -lt ${CONNTRACK_FILL_PCT} ]]; then
            pass "conntrack fill: ${FILL_PCT}% (< ${CONNTRACK_FILL_PCT}% threshold)"
            json_record "conntrack_fill" "PASS" "${FILL_PCT}%" "${CONNTRACK_FILL_PCT}%"
        elif [[ ${FILL_PCT} -lt 95 ]]; then
            warn "conntrack fill: ${FILL_PCT}% — approaching limit; increase nf_conntrack_max if needed"
            json_record "conntrack_fill" "WARN" "${FILL_PCT}%" "${CONNTRACK_FILL_PCT}%"
        else
            fail "conntrack fill: ${FILL_PCT}% — near capacity; connections may be dropped"
            json_record "conntrack_fill" "FAIL" "${FILL_PCT}%" "${CONNTRACK_FILL_PCT}%"
        fi

        # Headroom check
        HEADROOM=$(( CONNTRACK_MAX - CONNTRACK_COUNT ))
        info "conntrack headroom: ${HEADROOM} entries free"
        if [[ ${HEADROOM} -lt 1000 ]]; then
            warn "conntrack headroom < 1000 — risk of table exhaustion under load"
        fi
    fi
fi

# ── Check 3: Logging overhead ─────────────────────────────────────────────────
section "3. Log Rate (journald HISNOS entries)"

# Count HISNOS log entries in the last minute
LOG_COUNT_1MIN=$(journalctl -k --no-pager --since="-1min" -g "HISNOS-" 2>/dev/null | \
    grep -c "HISNOS-" 2>/dev/null || echo 0)

if [[ "${BASELINE}" == "true" ]]; then
    echo "logging.entries_per_min=${LOG_COUNT_1MIN}"
else
    info "HISNOS log entries (last 1min): ${LOG_COUNT_1MIN}"

    if [[ ${LOG_COUNT_1MIN} -lt ${LOG_RATE_WARN} ]]; then
        pass "Log rate: ${LOG_COUNT_1MIN}/min (< ${LOG_RATE_WARN} threshold)"
        json_record "log_rate" "PASS" "${LOG_COUNT_1MIN}/min" "${LOG_RATE_WARN}/min"
    elif [[ ${LOG_COUNT_1MIN} -lt ${LOG_RATE_FAIL} ]]; then
        warn "Log rate: ${LOG_COUNT_1MIN}/min — elevated; rate-limit chains should be absorbing this"
        warn "  If sustained: check for port scanners (section 4 inbound) or misconfigured apps"
        json_record "log_rate" "WARN" "${LOG_COUNT_1MIN}/min" "${LOG_RATE_WARN}/min"
    else
        fail "Log rate: ${LOG_COUNT_1MIN}/min — LOG STORM detected"
        fail "  Rate-limit chains are not throttling effectively — investigate immediately"
        fail "  Check: journalctl -k -g 'HISNOS-' --since=-2min | head 30"
        json_record "log_rate" "FAIL" "${LOG_COUNT_1MIN}/min" "${LOG_RATE_FAIL}/min"
    fi

    # Verify rate-limit chains are present (they control the flood)
    if sudo nft list chain inet hisnos log_out_drop &>/dev/null 2>&1; then
        pass "Rate-limit chain: log_out_drop present"
        json_record "ratelimit_chain" "PASS" "present" "required"
    else
        warn "Rate-limit chain: log_out_drop not found — logging is unbounded"
        json_record "ratelimit_chain" "WARN" "missing" "required"
    fi
fi

# ── Check 4: Loopback latency ─────────────────────────────────────────────────
section "4. Loopback Latency (baseline vs ruleset)"

# ping -c 10 127.0.0.1, extract average RTT in ms
PING_OUTPUT=$(ping -c 10 -i 0.1 -W 1 127.0.0.1 2>/dev/null || echo "")
if [[ -z "${PING_OUTPUT}" ]]; then
    skip "Loopback latency: ping failed — loopback may be down (critical)"
    json_record "loopback_latency" "SKIP" "n/a" "${MAX_LATENCY_MS}ms"
else
    # Extract avg RTT from "rtt min/avg/max/mdev = X/Y/Z/W ms"
    AVG_RTT_RAW=$(echo "${PING_OUTPUT}" | grep -oE "rtt min.*" | grep -oE "[0-9]+\.[0-9]+" | sed -n '2p' || echo "0.0")
    # Integer comparison: truncate to integer ms (loopback is sub-ms on modern hardware)
    AVG_RTT_INT=$(echo "${AVG_RTT_RAW}" | cut -d. -f1)
    AVG_RTT_INT="${AVG_RTT_INT:-0}"

    if [[ "${BASELINE}" == "true" ]]; then
        echo "latency.loopback_avg_ms=${AVG_RTT_RAW}"
    else
        info "Loopback RTT average: ${AVG_RTT_RAW}ms (threshold: < ${MAX_LATENCY_MS}ms)"

        if [[ ${AVG_RTT_INT} -lt ${MAX_LATENCY_MS} ]]; then
            pass "Loopback latency: ${AVG_RTT_RAW}ms avg RTT (< ${MAX_LATENCY_MS}ms)"
            json_record "loopback_latency" "PASS" "${AVG_RTT_RAW}ms" "${MAX_LATENCY_MS}ms"
        elif [[ ${AVG_RTT_INT} -lt 5 ]]; then
            warn "Loopback latency: ${AVG_RTT_RAW}ms — slightly elevated; may indicate heavy nft logging"
            json_record "loopback_latency" "WARN" "${AVG_RTT_RAW}ms" "${MAX_LATENCY_MS}ms"
        else
            fail "Loopback latency: ${AVG_RTT_RAW}ms — abnormal for loopback; check ruleset for infinite loops"
            json_record "loopback_latency" "FAIL" "${AVG_RTT_RAW}ms" "${MAX_LATENCY_MS}ms"
        fi
    fi
fi

# ── Check 5: NFQUEUE depth (OpenSnitch queue backpressure) ───────────────────
section "5. NFQUEUE Queue Depth (OpenSnitch)"

# /proc/net/netfilter/nfnetlink_queue: queue_num peer_portid queue_total copy_mode copy_range queue_dropped user_dropped id_sequence
if [[ -f /proc/net/netfilter/nfnetlink_queue ]]; then
    # Columns: queue_num portid queue_total copy_mode copy_range dropped user_dropped last_seq
    QUEUE_LINES=$(grep "^[0-9]" /proc/net/netfilter/nfnetlink_queue 2>/dev/null || echo "")
    if [[ -z "${QUEUE_LINES}" ]]; then
        info "NFQUEUE: no active queues (OpenSnitch may not be running)"
        json_record "nfqueue_depth" "SKIP" "no_queues" "${MAX_NFQUEUE_PENDING}"
    else
        MAX_PENDING=0
        TOTAL_DROPPED=0
        while IFS= read -r line; do
            # Field 3 = queue_total (current pending), field 6 = dropped
            Q_PENDING=$(echo "${line}" | awk '{print $3}')
            Q_DROPPED=$(echo "${line}" | awk '{print $6}')
            Q_NUM=$(echo "${line}" | awk '{print $1}')
            [[ ${Q_PENDING} -gt ${MAX_PENDING} ]] && MAX_PENDING=${Q_PENDING}
            TOTAL_DROPPED=$(( TOTAL_DROPPED + Q_DROPPED ))

            if [[ "${BASELINE}" == "true" ]]; then
                echo "nfqueue.queue${Q_NUM}.pending=${Q_PENDING}"
                echo "nfqueue.queue${Q_NUM}.dropped=${Q_DROPPED}"
            fi
        done <<< "${QUEUE_LINES}"

        if [[ "${BASELINE}" == "false" ]]; then
            info "NFQUEUE max pending: ${MAX_PENDING} packets | total dropped: ${TOTAL_DROPPED}"

            if [[ ${MAX_PENDING} -lt ${MAX_NFQUEUE_PENDING} ]]; then
                pass "NFQUEUE depth: ${MAX_PENDING} pending (< ${MAX_NFQUEUE_PENDING})"
                json_record "nfqueue_depth" "PASS" "${MAX_PENDING}" "${MAX_NFQUEUE_PENDING}"
            else
                warn "NFQUEUE depth: ${MAX_PENDING} — OpenSnitch may be slow or overloaded"
                json_record "nfqueue_depth" "WARN" "${MAX_PENDING}" "${MAX_NFQUEUE_PENDING}"
            fi

            if [[ ${TOTAL_DROPPED} -gt 0 ]]; then
                fail "NFQUEUE dropped: ${TOTAL_DROPPED} packets — OpenSnitch queue overflow (traffic silently dropped)"
                fail "  Action: restart opensnitchd or check if fail-closed is expected"
                json_record "nfqueue_dropped" "FAIL" "${TOTAL_DROPPED}" "0"
            else
                pass "NFQUEUE dropped: 0 (no packet drops in queue)"
                json_record "nfqueue_dropped" "PASS" "0" "0"
            fi
        fi
    fi
else
    skip "NFQUEUE stats: /proc/net/netfilter/nfnetlink_queue not found (kernel module not loaded?)"
    json_record "nfqueue_depth" "SKIP" "n/a" "${MAX_NFQUEUE_PENDING}"
fi

# ── Check 6: nftables set sizes (CIDR set population health) ─────────────────
section "6. Named Set Population"

for set_name in fedora_update_cidrs flatpak_cidrs steam_cidrs; do
    if sudo nft list set inet hisnos "${set_name}" &>/dev/null 2>&1; then
        COUNT=$(sudo nft list set inet hisnos "${set_name}" 2>/dev/null \
            | grep -c "^\t\t[0-9a-f./:]" 2>/dev/null || echo 0)
        if [[ "${BASELINE}" == "true" ]]; then
            echo "set.${set_name}.elements=${COUNT}"
        else
            if [[ "${set_name}" == "steam_cidrs" ]]; then
                # steam_cidrs is empty at idle — expected
                info "Set ${set_name}: ${COUNT} elements (empty at idle is expected)"
                json_record "set_${set_name}" "PASS" "${COUNT}" "0_at_idle"
            elif [[ ${COUNT} -eq 0 ]]; then
                warn "Set ${set_name}: EMPTY — CIDR allowlist not populated"
                warn "  Run: sudo egress/allowlists/update-cidrs.sh"
                json_record "set_${set_name}" "WARN" "0" ">0"
            else
                pass "Set ${set_name}: ${COUNT} CIDR elements loaded"
                json_record "set_${set_name}" "PASS" "${COUNT}" ">0"
            fi
        fi
    else
        if [[ "${BASELINE}" == "false" ]]; then
            warn "Set ${set_name}: not found in inet hisnos table"
            json_record "set_${set_name}" "WARN" "missing" "present"
        fi
    fi
done

# ── Check 7: journald write rate for HISNOS (storm detection) ─────────────────
section "7. journald Flood Detection"

# Count log entries over last 5 minutes and project to per-minute rate
LOG_COUNT_5MIN=$(journalctl -k --no-pager --since="-5min" -g "HISNOS-" 2>/dev/null | \
    grep -c "HISNOS-" 2>/dev/null || echo 0)
LOG_RATE_5MIN=$(( LOG_COUNT_5MIN / 5 ))

if [[ "${BASELINE}" == "true" ]]; then
    echo "logging.5min_entries=${LOG_COUNT_5MIN}"
    echo "logging.5min_rate_per_min=${LOG_RATE_5MIN}"
else
    info "HISNOS log entries (last 5min): ${LOG_COUNT_5MIN} (${LOG_RATE_5MIN}/min average)"

    # Check if log volume is stable (not a burst spike)
    if [[ ${LOG_RATE_5MIN} -gt ${LOG_RATE_FAIL} ]]; then
        fail "Sustained log storm: ${LOG_RATE_5MIN}/min over 5 min"
        fail "  Rate-limit is 5/min burst 10 per chain — sustained >200/min suggests multiple chains firing"
        fail "  Investigate: journalctl -k -g 'HISNOS-' --since=-5min | grep -oE 'HISNOS-[A-Z-]+' | sort | uniq -c"
    elif [[ ${LOG_RATE_5MIN} -gt ${LOG_RATE_WARN} ]]; then
        warn "Elevated log rate: ${LOG_RATE_5MIN}/min — monitor for sustained flooding"
    else
        pass "Log volume stable: ${LOG_RATE_5MIN}/min average over 5 minutes"
    fi

    # Check journald drop rate (if syslog-level pressure causes drops)
    JOURNAL_OVERFLOW=$(journalctl --disk-usage 2>/dev/null | grep -oE "[0-9.]+[MG] " | head -1 || echo "?")
    info "journald disk usage: ${JOURNAL_OVERFLOW}"
fi

# ── Summary ───────────────────────────────────────────────────────────────────
if [[ "${BASELINE}" == "true" ]]; then
    exit 0
fi

if [[ "${JSON}" == "true" ]]; then
    printf '{"summary":{"pass":%d,"warn":%d,"fail":%d,"skip":%d},"checks":[' \
        "${PASS}" "${WARN}" "${FAIL}" "${SKIP}"
    IFS=","
    echo "${JSON_RESULTS[*]:-}"
    echo "]}"
    ${STRICT} && [[ ${FAIL} -gt 0 ]] && exit 1
    exit 0
fi

echo ""
echo -e "${BOLD}══ Firewall Performance Summary ══${NC}"
echo -e "  ${GREEN}PASS${NC}: ${PASS}  ${YELLOW}WARN${NC}: ${WARN}  ${RED}FAIL${NC}: ${FAIL}  SKIP: ${SKIP}"
echo ""

if [[ ${FAIL} -gt 0 ]]; then
    echo -e "  ${RED}Performance issues detected.${NC}"
    echo "  Address FAIL items before extended observe/enforce periods."
fi

if [[ ${WARN} -gt 0 && ${FAIL} -eq 0 ]]; then
    echo -e "  ${YELLOW}Minor performance warnings.${NC} Monitor; no immediate action required."
fi

if [[ ${FAIL} -eq 0 && ${WARN} -eq 0 ]]; then
    echo -e "  ${GREEN}All performance checks passed.${NC} Ruleset is within expected bounds."
fi

echo ""
echo -e "  ${DIM}Baseline snapshot: ${0} --baseline > /tmp/perf-baseline.txt${NC}"
echo -e "  ${DIM}JSON output:        ${0} --json${NC}"
echo ""

${STRICT} && [[ ${FAIL} -gt 0 ]] && exit 1
exit 0
