#!/usr/bin/env bash
# tests/stress/lib/common.sh — Shared utilities for HisnOS stress test framework
#
# Machine-readable output contract:
#   Every test function must call result() exactly once.
#   result() emits a JSON object to stdout.
#   Human-readable progress goes to stderr.
#   run-all.sh aggregates all result() outputs into a JSON array.

set -euo pipefail

readonly STRESS_LIB_LOADED=1
readonly STRESS_TIMESTAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# Colours (stderr only)
RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'; NC=$'\033[0m'
info()  { echo -e "${GREEN}[INFO]${NC}  $*" >&2; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*" >&2; }
err()   { echo -e "${RED}[ERR]${NC}   $*" >&2; }
sep()   { echo -e "── $* ──" >&2; }

# Emit a machine-readable JSON result to stdout.
# Usage: result <test_name> <pass|fail|skip|warn> <score> <details_json>
# Example: result "fw_block_rate" "pass" 100 '{"events":10,"duration_s":5}'
result() {
    local test_name="$1"
    local status="$2"   # pass|fail|skip|warn
    local score="$3"    # 0-100
    local details="${4:-{}}"
    local ts
    ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    printf '{"test":"%s","status":"%s","score":%s,"timestamp":"%s","details":%s}\n' \
        "${test_name}" "${status}" "${score}" "${ts}" "${details}"
}

# Check if a binary is available; emit skip result if not.
require_bin() {
    local bin="$1" test_name="${2:-}"
    if ! command -v "${bin}" &>/dev/null; then
        if [[ -n "${test_name}" ]]; then
            result "${test_name}" "skip" 0 "{\"reason\":\"${bin} not found\"}"
        fi
        return 1
    fi
    return 0
}

# Elapsed time in seconds (integer) since a given epoch time.
elapsed_since() {
    local since="$1"
    echo $(( $(date +%s) - since ))
}

# Read risk score from threat-state.json.
read_risk_score() {
    local state_file="/var/lib/hisnos/threat-state.json"
    python3 -c "import json; d=json.load(open('${state_file}')); print(d.get('risk_score',0))" \
        2>/dev/null || echo "0"
}

# Wait for threatd to emit a new evaluation (polls threat-state.json for timestamp change).
wait_threatd_eval() {
    local timeout="${1:-60}"
    local state_file="/var/lib/hisnos/threat-state.json"
    local prev_ts=""
    if [[ -f "${state_file}" ]]; then
        prev_ts=$(python3 -c "import json; print(json.load(open('${state_file}')).get('updated_at',''))" 2>/dev/null || echo "")
    fi
    local waited=0
    while [[ "${waited}" -lt "${timeout}" ]]; do
        sleep 2
        waited=$(( waited + 2 ))
        if [[ -f "${state_file}" ]]; then
            local cur_ts
            cur_ts=$(python3 -c "import json; print(json.load(open('${state_file}')).get('updated_at',''))" 2>/dev/null || echo "")
            if [[ "${cur_ts}" != "${prev_ts}" ]]; then
                return 0
            fi
        fi
    done
    return 1  # timeout
}

# Count lines matching a pattern in the current audit log.
count_audit_events() {
    local pattern="$1"
    local audit_file="/var/lib/hisnos/audit/current.jsonl"
    grep -c "${pattern}" "${audit_file}" 2>/dev/null || echo "0"
}
