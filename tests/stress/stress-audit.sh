#!/usr/bin/env bash
# tests/stress/stress-audit.sh — Audit Pipeline Flood & Integrity Test
#
# Tests:
#   1. logd-running         — verify hisnos-logd service is active
#   2. audit-write-rate     — flood hisnos journal tags, verify events appear in current.jsonl
#   3. audit-rotation-state — verify log rotation state is healthy (file size, segment count)
#   4. auditd-rules-loaded  — verify HisnOS auditd rules are installed and active
#   5. audit-file-integrity — verify current.jsonl contains valid JSON lines (no corruption)
#
# Output: machine-readable JSON (one result object per line, to stdout)
# Progress: stderr

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

readonly AUDIT_DIR="/var/lib/hisnos/audit"
readonly AUDIT_CURRENT="${AUDIT_DIR}/current.jsonl"
readonly LOGD_UNIT="hisnos-logd.service"
readonly FLOOD_COUNT=50       # journal entries to emit
readonly FLOOD_TAG="hisnos-vault"  # use a known hisnos tag
readonly FLOOD_WAIT_SEC=15    # seconds to wait for logd to ingest

# ── Test 1: Logd running ──────────────────────────────────────────────────────

test_logd_running() {
    sep "Test: logd-running"
    info "Checking hisnos-logd service status..."

    local logd_active="false"
    local logd_pid=""
    local logd_memory=""

    if systemctl --user is-active "${LOGD_UNIT}" &>/dev/null; then
        logd_active="true"
        logd_pid=$(systemctl --user show "${LOGD_UNIT}" --property=MainPID \
            2>/dev/null | awk -F= '{print $2}' || echo "")
        logd_memory=$(systemctl --user show "${LOGD_UNIT}" --property=MemoryCurrent \
            2>/dev/null | awk -F= '{print $2}' || echo "")
    fi

    local details
    details=$(printf '{"logd_active":%s,"pid":"%s","memory_bytes":"%s"}' \
        "${logd_active}" "${logd_pid}" "${logd_memory}")

    info "  logd_active=${logd_active} pid=${logd_pid}"

    [[ "${logd_active}" == "true" ]] && \
        result "logd_running" "pass" 100 "${details}" || \
        result "logd_running" "fail" 0 "${details}"
}

# ── Test 2: Audit write rate (flood test) ─────────────────────────────────────

test_audit_write_rate() {
    sep "Test: audit-write-rate"

    if [[ ! -d "${AUDIT_DIR}" ]]; then
        result "audit_write_rate" "skip" 0 '{"reason":"audit directory does not exist"}'
        return
    fi

    local count_before=0
    [[ -f "${AUDIT_CURRENT}" ]] && count_before=$(wc -l < "${AUDIT_CURRENT}" 2>/dev/null || echo "0")
    info "Lines in current.jsonl before flood: ${count_before}"

    # Emit flood entries via systemd-cat using a known HisnOS syslog tag
    info "Emitting ${FLOOD_COUNT} journal entries via systemd-cat (tag: ${FLOOD_TAG})..."
    local i
    for (( i=1; i<=FLOOD_COUNT; i++ )); do
        systemd-cat -t "${FLOOD_TAG}" -p info \
            printf 'HISNOS_AUDIT_TEST=flood HISNOS_SEQ=%d msg="audit flood test entry"' "${i}" \
            2>/dev/null || true
    done
    local emit_ts
    emit_ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)

    # Wait for logd to ingest
    info "Waiting ${FLOOD_WAIT_SEC}s for logd to ingest entries..."
    sleep "${FLOOD_WAIT_SEC}"

    local count_after=0
    [[ -f "${AUDIT_CURRENT}" ]] && count_after=$(wc -l < "${AUDIT_CURRENT}" 2>/dev/null || echo "0")
    local delta=$(( count_after - count_before ))

    info "Lines after flood: ${count_after} (delta: ${delta})"

    # We expect at least half the flood entries to appear (logd may filter some)
    local expected_min=$(( FLOOD_COUNT / 2 ))
    local score=0
    [[ "${delta}" -ge "${expected_min}" ]] && score=100
    [[ "${delta}" -gt 0 && "${delta}" -lt "${expected_min}" ]] && score=50

    local details
    details=$(printf '{"flooded":%d,"lines_before":%d,"lines_after":%d,"delta":%d,"expected_min":%d,"emit_ts":"%s"}' \
        "${FLOOD_COUNT}" "${count_before}" "${count_after}" "${delta}" "${expected_min}" "${emit_ts}")

    if [[ "${delta}" -ge "${expected_min}" ]]; then
        result "audit_write_rate" "pass" "${score}" "${details}"
    elif [[ "${delta}" -gt 0 ]]; then
        warn "  Only ${delta}/${FLOOD_COUNT} entries ingested — logd may be filtering or slow"
        result "audit_write_rate" "warn" "${score}" "${details}"
    else
        warn "  No new lines appeared in current.jsonl — logd may not be running"
        result "audit_write_rate" "fail" 0 "${details}"
    fi
}

# ── Test 3: Rotation state ────────────────────────────────────────────────────

test_audit_rotation_state() {
    sep "Test: audit-rotation-state"
    info "Checking log rotation health..."

    if [[ ! -d "${AUDIT_DIR}" ]]; then
        result "audit_rotation_state" "skip" 0 '{"reason":"audit directory does not exist"}'
        return
    fi

    local current_size_bytes=0
    local current_size_mb=0
    local segment_count=0
    local oldest_gz=""
    local current_exists="false"

    if [[ -f "${AUDIT_CURRENT}" ]]; then
        current_exists="true"
        current_size_bytes=$(stat -c%s "${AUDIT_CURRENT}" 2>/dev/null || echo "0")
        current_size_mb=$(( current_size_bytes / 1048576 ))
    fi

    # Count compressed segments
    segment_count=$(find "${AUDIT_DIR}" -maxdepth 1 -name "hisnos-audit-*.jsonl.gz" 2>/dev/null | wc -l || echo "0")

    if [[ "${segment_count}" -gt 0 ]]; then
        oldest_gz=$(find "${AUDIT_DIR}" -maxdepth 1 -name "hisnos-audit-*.jsonl.gz" \
            2>/dev/null | sort | head -1 | xargs basename 2>/dev/null || echo "")
    fi

    info "  current.jsonl exists=${current_exists} size=${current_size_mb}MB"
    info "  compressed segments: ${segment_count} oldest=${oldest_gz}"

    # Warn if current.jsonl is approaching rotation threshold (50MB)
    local score=100
    [[ "${current_size_mb}" -gt 45 ]] && score=70  # within 5MB of rotation
    [[ "${current_size_mb}" -gt 55 ]] && score=40  # over limit — rotation may have failed

    local details
    details=$(printf '{"current_exists":%s,"current_size_mb":%d,"current_size_bytes":%d,"segments":%d,"oldest_segment":"%s"}' \
        "${current_exists}" "${current_size_mb}" "${current_size_bytes}" "${segment_count}" "${oldest_gz}")

    if [[ "${current_size_mb}" -gt 55 ]]; then
        warn "  current.jsonl exceeds 55MB — rotation may be stuck"
        result "audit_rotation_state" "warn" "${score}" "${details}"
    else
        result "audit_rotation_state" "pass" "${score}" "${details}"
    fi
}

# ── Test 4: Auditd rules loaded ───────────────────────────────────────────────

test_auditd_rules_loaded() {
    sep "Test: auditd-rules-loaded"
    info "Checking auditd service and HisnOS rules..."

    if ! require_bin "auditctl" "auditd_rules_loaded"; then return; fi

    local auditd_active="false"
    if systemctl is-active auditd.service &>/dev/null; then
        auditd_active="true"
    fi

    local hisnos_rules_count=0
    if [[ "${auditd_active}" == "true" ]]; then
        hisnos_rules_count=$(sudo auditctl -l 2>/dev/null | grep -c "hisnos_" || echo "0")
    fi

    local rules_file_present="false"
    [[ -f "/etc/audit/rules.d/hisnos.rules" ]] && rules_file_present="true"

    info "  auditd_active=${auditd_active} hisnos_rule_count=${hisnos_rules_count} rules_file=${rules_file_present}"

    local score=0
    [[ "${auditd_active}" == "true" ]] && score=$(( score + 40 ))
    [[ "${rules_file_present}" == "true" ]] && score=$(( score + 30 ))
    [[ "${hisnos_rules_count}" -gt 5 ]] && score=$(( score + 30 ))

    local details
    details=$(printf '{"auditd_active":%s,"hisnos_rules_loaded":%d,"rules_file_present":%s}' \
        "${auditd_active}" "${hisnos_rules_count}" "${rules_file_present}")

    if [[ "${auditd_active}" == "true" && "${hisnos_rules_count}" -gt 5 ]]; then
        result "auditd_rules_loaded" "pass" "${score}" "${details}"
    elif [[ "${auditd_active}" == "false" ]]; then
        result "auditd_rules_loaded" "fail" "${score}" "${details}"
    else
        warn "  auditd running but HisnOS rules may not be loaded"
        result "auditd_rules_loaded" "warn" "${score}" "${details}"
    fi
}

# ── Test 5: Audit file integrity ──────────────────────────────────────────────

test_audit_file_integrity() {
    sep "Test: audit-file-integrity"
    info "Validating JSON integrity of current.jsonl..."

    if [[ ! -f "${AUDIT_CURRENT}" ]]; then
        result "audit_file_integrity" "skip" 0 '{"reason":"current.jsonl does not exist"}'
        return
    fi

    local total_lines=0
    local valid_lines=0
    local invalid_lines=0
    local empty_lines=0

    # Validate up to last 1000 lines (avoid reading huge files)
    local check_lines=1000
    while IFS= read -r line; do
        [[ -z "${line}" ]] && (( empty_lines++ )) && continue
        (( total_lines++ )) || true
        if echo "${line}" | python3 -c "import json,sys; json.loads(sys.stdin.read())" 2>/dev/null; then
            (( valid_lines++ )) || true
        else
            (( invalid_lines++ )) || true
            warn "  Invalid JSON line: ${line:0:80}..."
        fi
    done < <(tail -n "${check_lines}" "${AUDIT_CURRENT}" 2>/dev/null)

    local score=100
    [[ "${total_lines}" -eq 0 ]] && score=50  # file exists but no parseable lines
    [[ "${invalid_lines}" -gt 0 ]] && score=$(( (valid_lines * 100) / (total_lines + 1) ))

    info "  Checked ${total_lines} lines: valid=${valid_lines} invalid=${invalid_lines} empty=${empty_lines}"

    local details
    details=$(printf '{"lines_checked":%d,"valid":%d,"invalid":%d,"empty":%d,"integrity_pct":%d}' \
        "${total_lines}" "${valid_lines}" "${invalid_lines}" "${empty_lines}" "${score}")

    if [[ "${invalid_lines}" -eq 0 && "${total_lines}" -gt 0 ]]; then
        result "audit_file_integrity" "pass" "${score}" "${details}"
    elif [[ "${invalid_lines}" -gt 0 ]]; then
        warn "  ${invalid_lines} invalid JSON lines found — possible mid-write corruption"
        result "audit_file_integrity" "warn" "${score}" "${details}"
    else
        result "audit_file_integrity" "warn" 50 "${details}"
    fi
}

# ── Main ──────────────────────────────────────────────────────────────────────

sep "HisnOS Audit Pipeline Flood & Integrity Test"
test_logd_running
test_audit_write_rate
test_audit_rotation_state
test_auditd_rules_loaded
test_audit_file_integrity
