#!/usr/bin/env bash
# installer/calamares/hisnos-post-verify.sh
#
# Post-install verification script.
# Runs inside the installed system chroot after bootstrap-installer.sh completes.
#
# Verifies:
#   1) rpm-ostree deployment is present
#   2) Core HisnOS services are enabled
#   3) User groups exist
#   4) Firewall rules file is installed
#   5) Onboarding binary and service are installed
#
# Exit codes:
#   0 — all checks pass
#   1 — one or more checks failed (logged; Calamares shows failure page)

set -euo pipefail

LOG="/var/log/hisnos-install.log"
RESULT="/tmp/hisnos-post-verify-result.json"

log() { echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] [post-verify] $*" | tee -a "${LOG}"; }

FAILURES=()
PASSES=()

check() {
  local name="$1"
  local result="$2"   # "pass" or "fail"
  local msg="${3:-}"
  if [[ "${result}" == "pass" ]]; then
    PASSES+=("${name}")
    log "PASS: ${name}${msg:+ — ${msg}}"
  else
    FAILURES+=("${name}: ${msg}")
    log "FAIL: ${name}${msg:+ — ${msg}}"
  fi
}

# ── 1. rpm-ostree deployment ──────────────────────────────────────────────────
if rpm-ostree status &>/dev/null; then
  DEPLOY_COUNT=$(rpm-ostree status --json 2>/dev/null | grep -c '"booted"' || echo 0)
  if [[ "${DEPLOY_COUNT}" -gt 0 ]]; then
    check "rpm-ostree deployment" "pass" "deployment exists"
  else
    check "rpm-ostree deployment" "fail" "no deployment found in rpm-ostree status"
  fi
else
  check "rpm-ostree deployment" "fail" "rpm-ostree command unavailable"
fi

# ── 2. Core system services enabled ──────────────────────────────────────────
SYSTEM_SERVICES=(
  "nftables.service"
  "auditd.service"
)
for svc in "${SYSTEM_SERVICES[@]}"; do
  if systemctl is-enabled --quiet "${svc}" 2>/dev/null; then
    check "service:${svc}" "pass"
  else
    check "service:${svc}" "fail" "not enabled — run: systemctl enable ${svc}"
  fi
done

# ── 3. HisnOS user service units installed ────────────────────────────────────
USER_UNITS=(
  "/usr/lib/systemd/user/hisnos-onboarding.service"
  "/usr/lib/systemd/user/hisnos-threatd.service"
  "/usr/lib/systemd/user/hisnosd.service"
)
for u in "${USER_UNITS[@]}"; do
  if [[ -f "${u}" ]]; then
    check "unit:$(basename "${u}")" "pass"
  else
    check "unit:$(basename "${u}")" "fail" "${u} not found"
  fi
done

# ── 4. User groups exist ──────────────────────────────────────────────────────
REQUIRED_GROUPS=("hisnos-gaming" "hisnos-lab")
for grp in "${REQUIRED_GROUPS[@]}"; do
  if getent group "${grp}" &>/dev/null; then
    check "group:${grp}" "pass"
  else
    check "group:${grp}" "fail" "group '${grp}' not found — run: groupadd ${grp}"
  fi
done

# ── 5. nftables rules file installed ─────────────────────────────────────────
NFT_CONF="/etc/nftables.conf"
if [[ -f "${NFT_CONF}" ]]; then
  if nft -c -f "${NFT_CONF}" &>/dev/null; then
    check "nftables config" "pass" "syntax valid"
  else
    check "nftables config" "fail" "syntax error in ${NFT_CONF}"
  fi
else
  check "nftables config" "fail" "${NFT_CONF} not found"
fi

# ── 6. Onboarding binary installed ───────────────────────────────────────────
ONBOARDING_BIN="/usr/local/bin/hisnos-onboarding"
if [[ -x "${ONBOARDING_BIN}" ]]; then
  check "onboarding binary" "pass"
else
  check "onboarding binary" "fail" "${ONBOARDING_BIN} not found or not executable"
fi

# ── Result JSON ───────────────────────────────────────────────────────────────
OVERALL=$( [[ "${#FAILURES[@]}" -eq 0 ]] && echo "true" || echo "false" )

pass_json="[$(for i in "${!PASSES[@]}"; do [[ $i -gt 0 ]] && echo -n ","; echo -n "\"${PASSES[$i]}\""; done)]"
fail_json="[$(for i in "${!FAILURES[@]}"; do [[ $i -gt 0 ]] && echo -n ","; echo -n "\"${FAILURES[$i]}\""; done)]"

cat > "${RESULT}" << JSONEOF
{
  "pass": ${OVERALL},
  "passed": ${pass_json},
  "failed": ${fail_json}
}
JSONEOF

log "Post-verify: pass=${OVERALL} passed=${#PASSES[@]} failed=${#FAILURES[@]}"

if [[ "${#FAILURES[@]}" -gt 0 ]]; then
  log "FAILURES DETECTED — installation may be incomplete:"
  for f in "${FAILURES[@]}"; do log "  - ${f}"; done
  exit 1
fi

log "All post-install checks passed."
exit 0
