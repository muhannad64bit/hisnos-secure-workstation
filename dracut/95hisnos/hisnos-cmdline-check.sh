#!/usr/bin/env bash
# dracut/95hisnos/hisnos-cmdline-check.sh
# Hook: pre-udev priority 05
#
# Validates the kernel command line against HisnOS policy before
# any hardware enumeration begins. This is the earliest possible
# enforcement point in the boot sequence.
#
# Policy enforcement:
#   - Detects and logs unauthorised kernel arguments
#   - Sets /run/hisnos/safemode flag if hisnos.safemode=1
#   - Sets /run/hisnos/recovery flag if hisnos.recovery=1
#   - Blocks kexec-unsafe boot if hisnos.strict=1 is set
#
# Unauthorised args (configurable via /etc/hisnos/cmdline-policy.conf):
#   nosecurity  noapic  nokaslr  ima_appraise=fix  apparmor=0
#   selinux=0   init=/bin/sh   rd.break (in strict mode)

. /hisnos-lib.sh 2>/dev/null || true

POLICY_FILE="/etc/hisnos/cmdline-policy.conf"
CMDLINE="$(cat /proc/cmdline 2>/dev/null)"
VIOLATIONS=0

hisnos_log INFO "Kernel cmdline check starting"

# ─── Set state flags ──────────────────────────────────────────────────────────
if echo "$CMDLINE" | grep -qw "hisnos.safemode=1" 2>/dev/null; then
    hisnos_flag set safemode 1
    hisnos_log WARN "Safe-mode requested via kernel cmdline"
fi

if echo "$CMDLINE" | grep -qw "hisnos.recovery=1" 2>/dev/null; then
    hisnos_flag set recovery 1
    hisnos_log WARN "Recovery mode requested via kernel cmdline"
fi

# In recovery mode we skip all policy enforcement
if hisnos_flag isset recovery; then
    hisnos_log INFO "Recovery mode active — skipping strict cmdline policy"
    exit 0
fi

# ─── Load policy ──────────────────────────────────────────────────────────────
# Default forbidden arguments
FORBIDDEN_ARGS=(
    "nosecurity"
    "apparmor=0"
    "selinux=0"
    "ima_appraise=fix"
    "efi=noruntime"
    "init=/bin/sh"
    "init=/bin/bash"
    "init=/sbin/sh"
)

STRICT_FORBIDDEN_ARGS=(
    "nokaslr"
    "noapic"
    "rd.break"
    "rd.shell"
    "single"
    "emergency"
    "systemd.debug-shell"
)

STRICT_MODE=false
if echo "$CMDLINE" | grep -qw "hisnos.strict=1" 2>/dev/null; then
    STRICT_MODE=true
    hisnos_log INFO "Strict cmdline enforcement active"
fi

# Load additional policy from config file
if [[ -f "$POLICY_FILE" ]]; then
    while IFS= read -r line; do
        [[ "$line" =~ ^#|^[[:space:]]*$ ]] && continue
        key="${line%%=*}"
        val="${line#*=}"
        case "$key" in
            forbidden_arg) FORBIDDEN_ARGS+=("$val") ;;
            strict_arg)    STRICT_FORBIDDEN_ARGS+=("$val") ;;
        esac
    done < "$POLICY_FILE"
fi

# ─── Check forbidden args ─────────────────────────────────────────────────────
for arg in "${FORBIDDEN_ARGS[@]}"; do
    if echo "$CMDLINE" | grep -qw "$arg" 2>/dev/null; then
        hisnos_log ERROR "POLICY VIOLATION: forbidden argument detected: $arg"
        logger -t hisnos-cmdline -p auth.alert "BOOT VIOLATION: $arg" 2>/dev/null || true
        VIOLATIONS=$((VIOLATIONS + 1))
    fi
done

if [[ "$STRICT_MODE" == "true" ]]; then
    for arg in "${STRICT_FORBIDDEN_ARGS[@]}"; do
        if echo "$CMDLINE" | grep -qw "$arg" 2>/dev/null; then
            hisnos_log ERROR "STRICT POLICY VIOLATION: $arg"
            logger -t hisnos-cmdline -p auth.alert "BOOT STRICT VIOLATION: $arg" 2>/dev/null || true
            VIOLATIONS=$((VIOLATIONS + 1))
        fi
    done
fi

# ─── Required args check ──────────────────────────────────────────────────────
# Audit must be enabled
if ! echo "$CMDLINE" | grep -qw "audit=1" 2>/dev/null; then
    hisnos_log WARN "audit=1 not present on kernel cmdline — security auditing may be inactive"
    hisnos_flag set audit_missing 1
fi

# ─── Violation response ───────────────────────────────────────────────────────
if [[ $VIOLATIONS -gt 0 ]]; then
    hisnos_flag set cmdline_violations "$VIOLATIONS"
    hisnos_log ERROR "${VIOLATIONS} kernel cmdline policy violation(s) detected"

    if [[ "$STRICT_MODE" == "true" ]]; then
        panic_banner "STRICT MODE: ${VIOLATIONS} policy violation(s) — see journalctl -b for details"
        # panic_banner reboots — we don't reach here
    else
        hisnos_log WARN "Non-strict mode: setting safemode flag and continuing"
        hisnos_flag set safemode 1
    fi
else
    hisnos_log OK "Kernel cmdline policy check passed"
fi
