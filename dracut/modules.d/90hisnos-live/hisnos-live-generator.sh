#!/bin/bash
# /usr/lib/systemd/system-generators/hisnos-live-generator
#
# systemd generator — runs early in the live system (after switch_root,
# before any services start).
#
# Responsibilities:
#   1. Detect live mode from /proc/cmdline and /etc/hisnos/.live-ok
#   2. Generate a hisnos-live-setup.service that creates /run/hisnos-live-ok
#   3. Generate a drop-in for multi-user.target to want hisnos-live-health.service
#   4. If NOT in live mode: generate nothing (no-op)
#
# systemd passes three directory arguments:
#   $1 = normal priority output dir (/run/systemd/generator)
#   $2 = early priority output dir  (/run/systemd/generator.early)
#   $3 = late priority output dir   (/run/systemd/generator.late)

NORMAL_DIR="${1:-/run/systemd/generator}"
EARLY_DIR="${2:-/run/systemd/generator.early}"
# LATE_DIR="${3:-/run/systemd/generator.late}"  # unused

# ── Detect live mode ───────────────────────────────────────────────────────
is_live_mode() {
    # Primary check: kernel cmdline.
    grep -q 'hisnos\.live=1' /proc/cmdline 2>/dev/null && return 0
    # Secondary check: marker written by dracut mount hook.
    [[ -f /etc/hisnos/.live-ok ]] && return 0
    return 1
}

is_live_mode || exit 0

# ── Ensure output directories exist ───────────────────────────────────────
mkdir -p "${NORMAL_DIR}" "${EARLY_DIR}" 2>/dev/null || exit 1

# ── hisnos-live-setup.service ──────────────────────────────────────────────
# Creates /run/hisnos-live-ok (the flag read by hisnos-installer.service)
# from /etc/hisnos/.live-ok (which survived switch_root in the overlayfs
# upper tier).  Runs before hisnos-live-health.service.
cat > "${EARLY_DIR}/hisnos-live-setup.service" <<'EOF'
[Unit]
Description=HisnOS Live Boot Setup (generator)
Documentation=file:///var/log/hisnos/dracut-live-boot.log
DefaultDependencies=no
Before=sysinit.target hisnos-live-health.service
After=local-fs.target

[Service]
Type=oneshot
RemainAfterExit=yes

# Create /run/hisnos/ with correct permissions.
ExecStart=/bin/mkdir -p /run/hisnos

# Verify the dracut live marker is present.
ExecStart=/bin/sh -c 'test -f /etc/hisnos/.live-ok || { echo "hisnos: .live-ok missing — not a live boot?" >&2; exit 1; }'

# Create the runtime flag that hisnos-installer.service checks.
# This flag is created HERE (inside live system's /run) so it is always
# fresh and cannot be faked by a stale file on disk.
ExecStart=/bin/touch /run/hisnos-live-ok

# Expose boot state for diagnostic tools.
ExecStart=/bin/sh -c 'grep -q hisnos.live=1 /proc/cmdline && echo HISNOS_LIVE_CMDLINE=1 >> /run/hisnos/env || true'
ExecStart=/bin/sh -c 'test -f /etc/hisnos/live-boot-state && cat /etc/hisnos/live-boot-state >> /run/hisnos/env || true'

StandardOutput=journal
StandardError=journal
SyslogIdentifier=hisnos-live-setup

[Install]
WantedBy=sysinit.target
EOF

# ── multi-user.target drop-in: want hisnos-live-health.service ────────────
mkdir -p "${NORMAL_DIR}/multi-user.target.wants" 2>/dev/null || true
# Symlink health service into multi-user.target.wants so it runs at every
# live boot without requiring it to be pre-installed in the squashfs.
ln -sf /usr/lib/systemd/system/hisnos-live-health.service \
    "${NORMAL_DIR}/multi-user.target.wants/hisnos-live-health.service" 2>/dev/null || true

# ── graphical.target drop-in: want hisnos-installer.service ──────────────
mkdir -p "${NORMAL_DIR}/graphical.target.wants" 2>/dev/null || true
ln -sf /usr/lib/systemd/system/hisnos-installer.service \
    "${NORMAL_DIR}/graphical.target.wants/hisnos-installer.service" 2>/dev/null || true

exit 0
