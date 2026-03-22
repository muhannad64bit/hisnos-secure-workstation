#!/usr/bin/env bash
# tests/stress/stress-update.sh — Update Lifecycle Rehearsal
#
# Tests:
#   1. preflight-check    — run hisnos-update-preflight.sh, verify it exits cleanly
#   2. update-status      — verify dashboard /api/update/status responds correctly
#   3. ostree-repo-state  — verify rpm-ostree is healthy and no pending deployment issues
#   4. rollback-available — verify at least one rollback target exists
#   5. update-script-lint — shellcheck update scripts if shellcheck is available
#
# This test rehearses the update lifecycle WITHOUT actually applying an update.
# All checks are read-only.
#
# Output: machine-readable JSON (one result object per line, to stdout)
# Progress: stderr

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

readonly HISNOS_DIR="${HOME}/.local/share/hisnos"
readonly PREFLIGHT_SCRIPT="${HISNOS_DIR}/update/hisnos-update-preflight.sh"
readonly UPDATE_SCRIPT="${HISNOS_DIR}/update/hisnos-update.sh"
readonly DASHBOARD_SOCKET="/run/user/$(id -u)/hisnos-dashboard.sock"
readonly DASHBOARD_URL="http://localhost:9443"

# ── Test 1: Preflight check ───────────────────────────────────────────────────

test_preflight_check() {
    sep "Test: preflight-check"

    if [[ ! -x "${PREFLIGHT_SCRIPT}" ]]; then
        result "preflight_check" "skip" 0 '{"reason":"hisnos-update-preflight.sh not installed"}'
        return
    fi

    info "Running preflight script (read-only check)..."
    local output exit_code start_ns end_ns elapsed_ms
    start_ns=$(date +%s%N)
    output=$("${PREFLIGHT_SCRIPT}" 2>&1) && exit_code=0 || exit_code=$?
    end_ns=$(date +%s%N)
    elapsed_ms=$(( (end_ns - start_ns) / 1000000 ))

    # Preflight should exit 0 (all clear) or 1 (warnings) — not 2+ (crash)
    local score=100
    local status_str="pass"
    case "${exit_code}" in
        0) info "  Preflight: all clear (exit 0)" ;;
        1) warn "  Preflight: warnings detected (exit 1)"; score=70; status_str="warn" ;;
        *) err "  Preflight: script error (exit ${exit_code})"; score=0; status_str="fail" ;;
    esac

    # Extract check summary from output (lines starting with OK/WARN/FAIL)
    local ok_count warn_count fail_count
    ok_count=$(echo "${output}" | grep -c "^\[OK\]\|^OK\b" 2>/dev/null || echo "0")
    warn_count=$(echo "${output}" | grep -c "^\[WARN\]\|^WARN\b" 2>/dev/null || echo "0")
    fail_count=$(echo "${output}" | grep -c "^\[FAIL\]\|^FAIL\b\|^\[ERR\]" 2>/dev/null || echo "0")

    local details
    details=$(printf '{"exit_code":%d,"elapsed_ms":%d,"ok":%d,"warn":%d,"fail":%d}' \
        "${exit_code}" "${elapsed_ms}" "${ok_count}" "${warn_count}" "${fail_count}")

    result "preflight_check" "${status_str}" "${score}" "${details}"
}

# ── Test 2: Update status API ─────────────────────────────────────────────────

test_update_status() {
    sep "Test: update-status"
    info "Querying dashboard /api/update/status..."

    if ! require_bin "curl" "update_status"; then return; fi

    # Try Unix socket first, then HTTP
    local response=""
    local http_code=""

    if [[ -S "${DASHBOARD_SOCKET}" ]]; then
        response=$(curl -sS --unix-socket "${DASHBOARD_SOCKET}" \
            -w "\n%{http_code}" \
            "http://localhost/api/update/status" 2>/dev/null || echo -e "\n000")
    else
        response=$(curl -sS -w "\n%{http_code}" \
            --max-time 5 \
            "${DASHBOARD_URL}/api/update/status" 2>/dev/null || echo -e "\n000")
    fi

    http_code=$(echo "${response}" | tail -1)
    local body
    body=$(echo "${response}" | head -n -1)

    info "  HTTP status: ${http_code}"

    if [[ "${http_code}" == "000" ]]; then
        result "update_status" "skip" 0 '{"reason":"dashboard not reachable"}'
        return
    fi

    local score=0
    local status_str="fail"
    if [[ "${http_code}" == "200" ]]; then
        score=100
        status_str="pass"
        # Verify response is valid JSON
        if ! echo "${body}" | python3 -c "import json,sys; json.loads(sys.stdin.read())" 2>/dev/null; then
            score=50
            status_str="warn"
            warn "  Response is not valid JSON"
        fi
    fi

    local details
    details=$(printf '{"http_code":%s,"response_valid_json":%s}' \
        "${http_code}" "$([[ "${score}" -eq 100 ]] && echo true || echo false)")

    result "update_status" "${status_str}" "${score}" "${details}"
}

# ── Test 3: OSTree repo state ─────────────────────────────────────────────────

test_ostree_repo_state() {
    sep "Test: ostree-repo-state"

    if ! require_bin "rpm-ostree" "ostree_repo_state"; then return; fi

    info "Checking rpm-ostree status..."
    local output exit_code
    output=$(rpm-ostree status --json 2>/dev/null) && exit_code=0 || exit_code=$?

    if [[ "${exit_code}" -ne 0 ]]; then
        result "ostree_repo_state" "fail" 0 \
            '{"reason":"rpm-ostree status failed","exit_code":'"${exit_code}"'}'
        return
    fi

    # Parse key fields
    local booted_version pending_version transaction_state
    booted_version=$(echo "${output}" | python3 -c \
        "import json,sys; d=json.load(sys.stdin); \
         deps=d.get('deployments',[]); \
         b=[x for x in deps if x.get('booted',False)]; \
         print(b[0].get('version','unknown') if b else 'none')" 2>/dev/null || echo "unknown")

    pending_version=$(echo "${output}" | python3 -c \
        "import json,sys; d=json.load(sys.stdin); \
         deps=d.get('deployments',[]); \
         p=[x for x in deps if not x.get('booted',False) and not x.get('rollback',False)]; \
         print(p[0].get('version','none') if p else 'none')" 2>/dev/null || echo "none")

    transaction_state=$(rpm-ostree status 2>/dev/null | grep -i "transaction\|state" | head -1 | \
        awk '{print $NF}' || echo "idle")

    local deployment_count
    deployment_count=$(echo "${output}" | python3 -c \
        "import json,sys; d=json.load(sys.stdin); print(len(d.get('deployments',[])))" \
        2>/dev/null || echo "0")

    info "  booted=${booted_version} pending=${pending_version} deployments=${deployment_count}"

    local score=100
    # Warn if a transaction is in progress (not idle)
    [[ "${transaction_state}" != "idle" && -n "${transaction_state}" ]] && score=70

    local details
    details=$(printf '{"booted_version":"%s","pending_version":"%s","deployment_count":%d,"transaction_state":"%s"}' \
        "${booted_version}" "${pending_version}" "${deployment_count}" "${transaction_state}")

    result "ostree_repo_state" "pass" "${score}" "${details}"
}

# ── Test 4: Rollback available ────────────────────────────────────────────────

test_rollback_available() {
    sep "Test: rollback-available"

    if ! require_bin "rpm-ostree" "rollback_available"; then return; fi

    info "Checking for rollback deployment..."

    local rollback_version="none"
    local rollback_count=0

    local output
    output=$(rpm-ostree status --json 2>/dev/null) || {
        result "rollback_available" "fail" 0 '{"reason":"rpm-ostree status failed"}'
        return
    }

    rollback_version=$(echo "${output}" | python3 -c \
        "import json,sys; d=json.load(sys.stdin); \
         deps=d.get('deployments',[]); \
         r=[x for x in deps if x.get('rollback',False)]; \
         print(r[0].get('version','none') if r else 'none')" 2>/dev/null || echo "none")

    rollback_count=$(echo "${output}" | python3 -c \
        "import json,sys; d=json.load(sys.stdin); \
         deps=d.get('deployments',[]); \
         print(len([x for x in deps if not x.get('booted',False)]))" \
        2>/dev/null || echo "0")

    info "  rollback_version=${rollback_version} non_booted_deployments=${rollback_count}"

    local score=0
    local status_str="fail"
    if [[ "${rollback_version}" != "none" || "${rollback_count}" -gt 0 ]]; then
        score=100
        status_str="pass"
    fi

    local details
    details=$(printf '{"rollback_version":"%s","non_booted_deployments":%d}' \
        "${rollback_version}" "${rollback_count}")

    if [[ "${status_str}" == "fail" ]]; then
        warn "  No rollback deployment available — first boot or cleaned deployments"
    fi

    result "rollback_available" "${status_str}" "${score}" "${details}"
}

# ── Test 5: Script lint ───────────────────────────────────────────────────────

test_update_script_lint() {
    sep "Test: update-script-lint"

    if ! require_bin "shellcheck" "update_script_lint"; then return; fi

    local scripts=()
    [[ -f "${PREFLIGHT_SCRIPT}" ]] && scripts+=("${PREFLIGHT_SCRIPT}")
    [[ -f "${UPDATE_SCRIPT}" ]] && scripts+=("${UPDATE_SCRIPT}")

    if [[ "${#scripts[@]}" -eq 0 ]]; then
        result "update_script_lint" "skip" 0 '{"reason":"no update scripts found to lint"}'
        return
    fi

    info "Running shellcheck on ${#scripts[@]} update script(s)..."
    local total=0 passed=0 failed=0

    for script in "${scripts[@]}"; do
        (( total++ )) || true
        local sc_output sc_exit
        sc_output=$(shellcheck --severity=warning "${script}" 2>&1) && sc_exit=0 || sc_exit=$?
        if [[ "${sc_exit}" -eq 0 ]]; then
            (( passed++ )) || true
            info "  $(basename "${script}"): ok"
        else
            (( failed++ )) || true
            local issue_count
            issue_count=$(echo "${sc_output}" | grep -c "^In " || echo "1")
            warn "  $(basename "${script}"): ${issue_count} issue(s)"
        fi
    done

    local score=$(( (passed * 100) / total ))
    local details
    details=$(printf '{"scripts_checked":%d,"passed":%d,"failed":%d}' \
        "${total}" "${passed}" "${failed}")

    [[ "${failed}" -eq 0 ]] && \
        result "update_script_lint" "pass" "${score}" "${details}" || \
        result "update_script_lint" "warn" "${score}" "${details}"
}

# ── Main ──────────────────────────────────────────────────────────────────────

sep "HisnOS Update Lifecycle Rehearsal"
test_preflight_check
test_update_status
test_ostree_repo_state
test_rollback_available
test_update_script_lint
