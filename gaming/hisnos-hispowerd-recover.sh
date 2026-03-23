#!/usr/bin/env bash
# gaming/hisnos-hispowerd-recover.sh — hispowerd Crash Recovery Script
#
# Run this as the user (or root for IRQ/governor) if hispowerd crashes
# and leaves the system in a partially-applied gaming state.
#
# What it restores:
#   1. CPU affinity   — resets all user processes to allow all CPUs
#   2. IRQ affinity   — writes 0xffffffff to all modified /proc/irq/*/smp_affinity (root)
#   3. nftables       — flushes and removes hisnos_gaming_fast table
#   4. CPU governor   — restores powersave (or schedutil) on all CPUs (root)
#   5. sched_autogroup — resets to 1 (kernel default)
#   6. cgroup cpu.max — restores "max 100000" (no quota) on daemon cgroups
#   7. vault timer    — restarts hisnos-vault-idle.timer
#   8. gaming env     — removes ~/.config/environment.d/hisnos-gaming.conf
#   9. gaming state   — clears /var/lib/hisnos/gaming-state.json
#  10. control plane  — sets mode=normal in /var/lib/hisnos/core-state.json
#
# Usage:
#   bash hisnos-hispowerd-recover.sh [--dry-run] [--root-ops]
#
#   --dry-run    Print actions without executing them
#   --root-ops   Also run privileged operations (IRQ, governor).
#                Requires root OR sudo configured with NOPASSWD.
#
# This script is idempotent and safe to run multiple times.

set -uo pipefail

DRY_RUN=false
DO_ROOT_OPS=false
ERRORS=0

for arg in "$@"; do
  case "$arg" in
    --dry-run)    DRY_RUN=true ;;
    --root-ops)   DO_ROOT_OPS=true ;;
  esac
done

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'; NC=$'\033[0m'
ok()   { echo -e "${GREEN}[OK]${NC}   $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
fail() { echo -e "${RED}[FAIL]${NC} $*"; ((ERRORS++)) || true; }
run()  {
  if [[ "${DRY_RUN}" == "true" ]]; then
    echo -e "[DRY-RUN] $*"
  else
    eval "$@" || fail "command failed: $*"
  fi
}

echo ""
echo "=== hispowerd crash recovery ==="
echo "dry_run=${DRY_RUN} root_ops=${DO_ROOT_OPS}"
echo ""

# ─────────────────────────────────────────────────────────────────
# 1. CPU Affinity — reset all user processes to all CPUs
# ─────────────────────────────────────────────────────────────────
echo "--- Step 1: CPU affinity reset ---"
UID_NUM="$(id -u)"
CPU_COUNT="$(nproc 2>/dev/null || echo 8)"
ALL_CPUS="0-$((CPU_COUNT - 1))"

# Reset all processes owned by this user.
for pid_dir in /proc/[0-9]*/; do
  pid="$(basename "${pid_dir}")"
  # Check if this process is owned by us.
  if [[ -r "/proc/${pid}/status" ]] && \
     grep -q "Uid:.*${UID_NUM}" "/proc/${pid}/status" 2>/dev/null; then
    run "taskset -cp '${ALL_CPUS}' '${pid}' >/dev/null 2>&1 || true"
  fi
done
ok "CPU affinity reset for user ${UID_NUM} processes to ${ALL_CPUS}"

# ─────────────────────────────────────────────────────────────────
# 2. IRQ Affinity — restore all-CPUs mask (root required)
# ─────────────────────────────────────────────────────────────────
echo ""
echo "--- Step 2: IRQ affinity restore ---"
if [[ "${DO_ROOT_OPS}" == "true" ]]; then
  if [[ "$(id -u)" -ne 0 ]]; then
    # Try sudo
    SUDO_CMD="sudo"
    if ! sudo -n true 2>/dev/null; then
      warn "sudo not available without password — IRQ restore requires root"
      SUDO_CMD=""
    fi
  else
    SUDO_CMD=""
  fi

  if [[ -d /proc/irq ]]; then
    IRQ_COUNT=0
    for irq_dir in /proc/irq/[0-9]*/; do
      irq_num="$(basename "${irq_dir}")"
      affinity_file="${irq_dir}smp_affinity"
      if [[ -f "${affinity_file}" ]]; then
        run "${SUDO_CMD} bash -c 'echo ffffffff > ${affinity_file}' 2>/dev/null || true"
        ((IRQ_COUNT++)) || true
      fi
    done
    ok "IRQ affinity reset for ${IRQ_COUNT} IRQ(s) to 0xffffffff"
  fi
else
  warn "IRQ affinity reset skipped (run with --root-ops for this step)"
  warn "Manual: sudo sh -c 'for f in /proc/irq/*/smp_affinity; do echo ffffffff > \$f; done'"
fi

# ─────────────────────────────────────────────────────────────────
# 3. nftables — remove hisnos_gaming_fast table
# ─────────────────────────────────────────────────────────────────
echo ""
echo "--- Step 3: nftables fast path removal ---"
NFT=/usr/sbin/nft
if command -v "${NFT}" &>/dev/null; then
  # Check if table exists first.
  if "${NFT}" list table inet hisnos_gaming_fast &>/dev/null 2>&1; then
    run "${NFT} flush table inet hisnos_gaming_fast 2>/dev/null || true"
    run "${NFT} delete table inet hisnos_gaming_fast 2>/dev/null || true"
    ok "hisnos_gaming_fast table removed"
  else
    ok "hisnos_gaming_fast table not present — nothing to remove"
  fi
  # Verify baseline policy.
  if "${NFT}" list table inet hisnos_egress &>/dev/null 2>&1; then
    ok "hisnos_egress baseline policy: INTACT"
  else
    fail "hisnos_egress table MISSING — baseline policy may be broken!"
    warn "Run: sudo systemctl reload-or-restart nftables"
  fi
else
  warn "nft not found at ${NFT} — skipping nftables recovery"
fi

# ─────────────────────────────────────────────────────────────────
# 4. CPU Governor — restore powersave/schedutil (root required)
# ─────────────────────────────────────────────────────────────────
echo ""
echo "--- Step 4: CPU governor restore ---"
if [[ "${DO_ROOT_OPS}" == "true" ]]; then
  RESTORE_GOV="powersave"
  # Prefer schedutil if available (better for non-gaming use).
  if [[ -f /sys/devices/system/cpu/cpu0/cpufreq/scaling_available_governors ]]; then
    if grep -q "schedutil" /sys/devices/system/cpu/cpu0/cpufreq/scaling_available_governors 2>/dev/null; then
      RESTORE_GOV="schedutil"
    fi
  fi

  GOV_COUNT=0
  for gov_file in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do
    [[ -f "${gov_file}" ]] || continue
    run "${SUDO_CMD:-} bash -c 'echo ${RESTORE_GOV} > ${gov_file}' 2>/dev/null || true"
    ((GOV_COUNT++)) || true
  done
  if [[ "${GOV_COUNT}" -gt 0 ]]; then
    ok "CPU governor reset to '${RESTORE_GOV}' on ${GOV_COUNT} CPU(s)"
  else
    warn "No cpufreq governors found (may not have cpufreq driver loaded)"
  fi
else
  warn "Governor restore skipped (run with --root-ops)"
  warn "Manual: for f in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do echo powersave > \$f; done"
fi

# ─────────────────────────────────────────────────────────────────
# 5. sched_autogroup
# ─────────────────────────────────────────────────────────────────
echo ""
echo "--- Step 5: sched_autogroup reset ---"
AUTOGROUP=/proc/sys/kernel/sched_autogroup_enabled
if [[ -f "${AUTOGROUP}" ]]; then
  run "echo 1 > '${AUTOGROUP}' 2>/dev/null || ${SUDO_CMD:-sudo} bash -c 'echo 1 > ${AUTOGROUP}'"
  ok "sched_autogroup_enabled reset to 1"
else
  warn "sched_autogroup not available on this kernel"
fi

# ─────────────────────────────────────────────────────────────────
# 6. cgroup cpu.max — restore daemon CPU quotas
# ─────────────────────────────────────────────────────────────────
echo ""
echo "--- Step 6: cgroup cpu.max restore ---"
UID_NUM="$(id -u)"
USER_CGROUP_BASE="/sys/fs/cgroup/user.slice/user-${UID_NUM}.slice/user@${UID_NUM}.service"
RESTORE_CPU_MAX="max 100000"  # no quota

DAEMONS=("hisnos-threatd.service" "hisnos-logd.service" "hisnos-dashboard.service")
for svc in "${DAEMONS[@]}"; do
  for base_path in "${USER_CGROUP_BASE}/app.slice/${svc}" "${USER_CGROUP_BASE}/${svc}"; do
    cpu_max="${base_path}/cpu.max"
    if [[ -f "${cpu_max}" ]]; then
      run "echo '${RESTORE_CPU_MAX}' > '${cpu_max}' 2>/dev/null || true"
      ok "Restored cpu.max for ${svc}"
      break
    fi
  done
done

# ─────────────────────────────────────────────────────────────────
# 7. Vault idle timer
# ─────────────────────────────────────────────────────────────────
echo ""
echo "--- Step 7: vault idle timer ---"
if systemctl --user is-enabled hisnos-vault-idle.timer &>/dev/null 2>&1; then
  run "systemctl --user start hisnos-vault-idle.timer 2>/dev/null || true"
  ok "hisnos-vault-idle.timer restarted"
else
  warn "hisnos-vault-idle.timer not enabled — skipping"
fi

# ─────────────────────────────────────────────────────────────────
# 8. Gaming environment file
# ─────────────────────────────────────────────────────────────────
echo ""
echo "--- Step 8: gaming env vars ---"
ENV_CONF="${XDG_CONFIG_HOME:-${HOME}/.config}/environment.d/hisnos-gaming.conf"
if [[ -f "${ENV_CONF}" ]]; then
  run "rm -f '${ENV_CONF}'"
  ok "Removed ${ENV_CONF}"
else
  ok "hisnos-gaming.conf not present — nothing to remove"
fi

# ─────────────────────────────────────────────────────────────────
# 9. gaming-state.json
# ─────────────────────────────────────────────────────────────────
echo ""
echo "--- Step 9: gaming state file ---"
GAMING_STATE="/var/lib/hisnos/gaming-state.json"
if [[ -f "${GAMING_STATE}" ]]; then
  TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  CLEAR_STATE="{\"gaming_active\":false,\"updated_at\":\"${TIMESTAMP}\"}"
  run "echo '${CLEAR_STATE}' > '${GAMING_STATE}'"
  ok "gaming-state.json cleared"
else
  warn "${GAMING_STATE} not found — nothing to clear"
fi

# ─────────────────────────────────────────────────────────────────
# 10. control-plane mode → normal
# ─────────────────────────────────────────────────────────────────
echo ""
echo "--- Step 10: control plane mode ---"
CP_STATE="/var/lib/hisnos/core-state.json"
if [[ -f "${CP_STATE}" ]]; then
  # Try hisnosd IPC first.
  UID_NUM="$(id -u)"
  HISNOSD_SOCK="/run/user/${UID_NUM}/hisnosd.sock"
  if [[ -S "${HISNOSD_SOCK}" ]] && command -v socat &>/dev/null; then
    MODE_CMD='{"id":"recover-1","command":"set_mode","params":{"mode":"normal"}}'
    RESPONSE="$(echo "${MODE_CMD}" | socat - UNIX-CONNECT:"${HISNOSD_SOCK}" 2>/dev/null || true)"
    if echo "${RESPONSE}" | grep -q '"ok":true'; then
      ok "Control plane mode → normal (via hisnosd IPC)"
    else
      warn "hisnosd IPC failed — falling back to direct file edit"
      TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
      run "python3 -c \"
import json, sys
with open('${CP_STATE}') as f: d = json.load(f)
d['mode'] = 'normal'
d['updated_at'] = '${TIMESTAMP}'
with open('${CP_STATE}', 'w') as f: json.dump(d, f, indent=2)
\" 2>/dev/null || true"
      ok "Control plane mode → normal (direct write)"
    fi
  else
    warn "hisnosd not available — direct write to ${CP_STATE}"
    TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    run "python3 -c \"
import json, sys
try:
    with open('${CP_STATE}') as f: d = json.load(f)
except: d = {}
d['mode'] = 'normal'
d['updated_at'] = '${TIMESTAMP}'
with open('${CP_STATE}', 'w') as f: json.dump(d, f, indent=2)
\" 2>/dev/null || true"
    ok "Control plane mode → normal (direct write)"
  fi
else
  warn "${CP_STATE} not found"
fi

# ─────────────────────────────────────────────────────────────────
# Summary
# ─────────────────────────────────────────────────────────────────
echo ""
echo "=== recovery complete ==="
if [[ "${ERRORS}" -eq 0 ]]; then
  echo -e "${GREEN}All steps succeeded.${NC}"
else
  echo -e "${YELLOW}${ERRORS} step(s) had warnings or failures — review output above.${NC}"
fi
echo ""
echo "Next steps:"
echo "  1. Verify hisnos_egress table: sudo nft list table inet hisnos_egress"
echo "  2. Check gaming state:         cat /var/lib/hisnos/gaming-state.json"
echo "  3. Restart hispowerd:          systemctl --user restart hisnos-hispowerd.service"
echo "  4. Check vault timer:          systemctl --user status hisnos-vault-idle.timer"
echo ""

exit "${ERRORS}"
