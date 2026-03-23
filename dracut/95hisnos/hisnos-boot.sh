#!/usr/bin/env bash
# dracut/95hisnos/hisnos-boot.sh
# Hook: pre-pivot priority 40
#
# Primary HisnOS boot health check and safe-mode gate.
# Runs after the real root is mounted at /sysroot but before pivot_root.
#
# Actions:
#   1. Read rolling boot score from installed system state
#   2. If score < CRITICAL_THRESHOLD → force safe-mode
#   3. Validate /sysroot filesystem integrity
#   4. Validate /etc/hisnos/release exists on target
#   5. Write /run/hisnos/boot-* flags for hisnosd startup
#   6. If safe-mode: write /sysroot/run/hisnos/safemode-active
#
# Boot score thresholds:
#   ≥ 60  → Nominal (continue normal boot)
#   40-59 → Warn    (set warn flag, continue)
#   < 40  → Critical (force safe-mode)
#   0     → Unknown  (first boot — skip scoring)

. /hisnos-lib.sh 2>/dev/null || true

SYSROOT="${NEWROOT:-/sysroot}"
HISNOS_STATE="${SYSROOT}/var/lib/hisnos"
BOOT_HEALTH="${HISNOS_STATE}/boot-health.json"
HISNOS_RELEASE="${SYSROOT}/etc/hisnos/release"

SCORE_WARN=60
SCORE_CRIT=40

hisnos_log INFO "Boot health check starting (pre-pivot)"

# ─── Step 1: Validate sysroot ─────────────────────────────────────────────────
if ! mountpoint -q "$SYSROOT" 2>/dev/null; then
    hisnos_log WARN "sysroot not yet mounted at $SYSROOT — skipping health check"
    exit 0
fi

# ─── Step 2: Validate HisnOS release ────────────────────────────────────────
if [[ ! -f "$HISNOS_RELEASE" ]]; then
    hisnos_log WARN "HisnOS release file not found: $HISNOS_RELEASE"
    hisnos_log WARN "This may be a first boot or non-HisnOS system"
    hisnos_flag set first_boot 1
else
    # Parse version
    HISNOS_VERSION="$(grep -oP '(?<=HISNOS_VERSION=)[^ ]+' "$HISNOS_RELEASE" 2>/dev/null || echo unknown)"
    hisnos_log INFO "HisnOS release: $HISNOS_VERSION"
    hisnos_flag set hisnos_version "$HISNOS_VERSION"
fi

# ─── Step 3: Read boot health score ──────────────────────────────────────────
SCORE=0
SCORE_SOURCE="none"

if [[ -f "$BOOT_HEALTH" ]]; then
    # Extract rolling_score from JSON without jq
    SCORE="$(grep -oP '"rolling_score":\s*\K[\d.]+' "$BOOT_HEALTH" 2>/dev/null | head -1 || echo 0)"
    SCORE="${SCORE%.*}"  # truncate decimal
    SCORE_SOURCE="boot-health.json"
    hisnos_log INFO "Boot health score: ${SCORE} (from ${SCORE_SOURCE})"
elif hisnos_flag isset first_boot; then
    hisnos_log INFO "First boot — boot score not yet available"
    SCORE=100
    SCORE_SOURCE="first-boot"
else
    hisnos_log WARN "Boot health state not found — assuming unknown"
    SCORE=0
    SCORE_SOURCE="missing"
fi

hisnos_flag set boot_score "$SCORE"
hisnos_flag set boot_score_source "$SCORE_SOURCE"

# ─── Step 4: Apply thresholds ────────────────────────────────────────────────
FORCE_SAFEMODE=false

if [[ "$SCORE_SOURCE" == "missing" ]]; then
    hisnos_log WARN "No boot history — will monitor this boot"
elif [[ $SCORE -ge $SCORE_WARN ]]; then
    hisnos_log OK "Boot reliability: GOOD (score=${SCORE})"
elif [[ $SCORE -ge $SCORE_CRIT ]]; then
    hisnos_log WARN "Boot reliability: DEGRADED (score=${SCORE} < threshold=${SCORE_WARN})"
    hisnos_flag set boot_warn 1
else
    hisnos_log ERROR "Boot reliability: CRITICAL (score=${SCORE} < threshold=${SCORE_CRIT})"
    hisnos_log ERROR "Forcing safe-mode due to consecutive boot failures"
    FORCE_SAFEMODE=true
    hisnos_flag set boot_critical 1
fi

# ─── Step 5: Check for explicit safemode flags ────────────────────────────────
if hisnos_flag isset safemode; then
    hisnos_log WARN "Safe-mode flag set (via cmdline or previous failure)"
    FORCE_SAFEMODE=true
fi

# ─── Step 6: Apply safe-mode ─────────────────────────────────────────────────
if [[ "$FORCE_SAFEMODE" == "true" ]]; then
    hisnos_log WARN "SAFE MODE ACTIVE"

    # Write safe-mode activation marker to target system
    mkdir -p "${SYSROOT}/run/hisnos" 2>/dev/null || true
    echo "1" > "${SYSROOT}/run/hisnos/safemode-active" 2>/dev/null || true

    # Record the boot score that triggered it
    cat > "${SYSROOT}/run/hisnos/safemode-reason.json" 2>/dev/null <<EOF || true
{
  "reason": "boot_reliability_critical",
  "score": ${SCORE},
  "threshold": ${SCORE_CRIT},
  "timestamp": "$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)"
}
EOF

    # Signal to systemd that we want rescue mode
    # (systemd reads /run/systemd/default-target)
    mkdir -p /run/systemd 2>/dev/null || true
    echo "rescue.target" > /run/systemd/default-target 2>/dev/null || true

    hisnos_log WARN "System will boot to rescue.target"

    # Give operator time to read the warning
    echo ""
    echo -e "${YELLOW}${BOLD}HisnOS is entering Safe Mode.${RESET}"
    echo -e "Boot reliability score: ${SCORE}/100 (critical threshold: ${SCORE_CRIT})"
    echo -e "The system will boot to rescue.target in 5 seconds."
    echo -e "Press ENTER to continue immediately, or add ${BOLD}hisnos.recovery=1${RESET} to kernel cmdline."
    echo ""
    read -r -t 5 _ 2>/dev/null || true
fi

# ─── Step 7: Filesystem integrity quick check ─────────────────────────────────
# Find the root block device
ROOT_DEV="$(findmnt -n -o SOURCE "$SYSROOT" 2>/dev/null | head -1)"
if [[ -n "$ROOT_DEV" ]]; then
    ROOT_FSTYPE="$(findmnt -n -o FSTYPE "$SYSROOT" 2>/dev/null | head -1)"
    hisnos_log INFO "Root: ${ROOT_DEV} (${ROOT_FSTYPE})"
    hisnos_flag set root_device "$ROOT_DEV"
    hisnos_flag set root_fstype "$ROOT_FSTYPE"

    # Only fsck ext4 (btrfs self-heals, xfs needs mount options)
    if [[ "$ROOT_FSTYPE" == "ext4" ]]; then
        hisnos_log INFO "Running read-only ext4 check on ${ROOT_DEV}..."
        if ! fsck -n "$ROOT_DEV" &>/dev/null; then
            hisnos_log WARN "Filesystem errors detected on ${ROOT_DEV}"
            hisnos_flag set fsck_errors 1
        else
            hisnos_log OK "Filesystem OK"
        fi
    fi
fi

# ─── Done ────────────────────────────────────────────────────────────────────
hisnos_log OK "Boot health check complete (score=${SCORE} safe_mode=${FORCE_SAFEMODE})"

# Record final boot summary
cat > "${HISNOS_RUN}/boot-summary.json" 2>/dev/null <<EOF || true
{
  "score": ${SCORE},
  "source": "${SCORE_SOURCE}",
  "safe_mode": ${FORCE_SAFEMODE},
  "first_boot": $(hisnos_flag isset first_boot && echo "true" || echo "false"),
  "boot_warn": $(hisnos_flag isset boot_warn && echo "true" || echo "false"),
  "boot_critical": $(hisnos_flag isset boot_critical && echo "true" || echo "false"),
  "timestamp": "$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo unknown)"
}
EOF
