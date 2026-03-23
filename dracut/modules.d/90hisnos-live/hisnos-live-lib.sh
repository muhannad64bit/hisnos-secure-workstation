#!/bin/bash
# /lib/dracut/hisnos-live-lib.sh — HisnOS Live Boot shared library.
# Sourced by all hisnos-live hook scripts inside the initramfs.
#
# Provides:
#   hisnos_log LEVEL "message"   — structured log to /run/hisnos/boot.log + console
#   hisnos_die "reason"          — log fatal error, launch emergency UI
#   hisnos_init_dirs             — create all required /run/hisnos/* directories
#   hisnos_find_source_dev       — locate the ISO block device (4 strategies)
#   hisnos_validate_root [path]  — sanity-check a candidate new root

# ── State paths ────────────────────────────────────────────────────────────
HISNOS_RUN=/run/hisnos
HISNOS_ISO_MNT=/run/hisnos/isodev
HISNOS_SQUASHFS_MNT=/run/hisnos/squashfs
HISNOS_OVERLAY_DIR=/run/hisnos/overlay
HISNOS_LIVE_IMG=/LiveOS/rootfs.img
HISNOS_LOG=/run/hisnos/live-boot.log
HISNOS_CDLABEL=${HISNOS_CDLABEL:-HISNOS_LIVE}
HISNOS_FAIL_REASON=/run/hisnos/fail-reason

# ANSI colours (disabled if not a colour-capable terminal).
_RED='\033[0;31m'; _GRN='\033[0;32m'; _YLW='\033[0;33m'
_CYN='\033[0;36m'; _RST='\033[0m'; _BLD='\033[1m'
if ! [ -t 2 ]; then _RED=''; _GRN=''; _YLW=''; _CYN=''; _RST=''; _BLD=''; fi

# ── Logging ────────────────────────────────────────────────────────────────
hisnos_log() {
    local level="$1"; shift
    local msg="$*"
    local ts; ts="$(date +%H:%M:%S 2>/dev/null || echo '??:??:??')"
    local entry="[${ts}][${level}] ${msg}"

    # Always append to log file (create if needed).
    mkdir -p "${HISNOS_RUN}" 2>/dev/null || true
    echo "${entry}" >> "${HISNOS_LOG}" 2>/dev/null || true

    # Emit to kernel ring buffer (survives across pivot).
    echo "hisnos-live: ${level}: ${msg}" > /dev/kmsg 2>/dev/null || true

    # Console output with colour.
    case "${level}" in
        FATAL|ERROR)
            printf "${_RED}${_BLD}!!! [hisnos-live] %s${_RST}\n" "${msg}" >&2 ;;
        WARN)
            printf "${_YLW}  * [hisnos-live] %s${_RST}\n" "${msg}" >&2 ;;
        OK)
            printf "${_GRN}  ✓ [hisnos-live] %s${_RST}\n" "${msg}" ;;
        INFO)
            printf "${_CYN}  → [hisnos-live] %s${_RST}\n" "${msg}" ;;
        *)
            printf "    [hisnos-live] %s\n" "${msg}" ;;
    esac
}

# ── Fatal exit ─────────────────────────────────────────────────────────────
# Called on any unrecoverable error.  Logs the reason, persists it for the
# emergency UI to display, then executes hisnos-emergency.
hisnos_die() {
    local reason="$*"
    hisnos_log FATAL "${reason}"
    mkdir -p "${HISNOS_RUN}" 2>/dev/null || true
    printf '%s\n' "${reason}" > "${HISNOS_FAIL_REASON}" 2>/dev/null || true
    # Copy log so emergency shell can read it even if /run/hisnos is messy.
    cp "${HISNOS_LOG}" /tmp/hisnos-live-boot.log 2>/dev/null || true
    exec /bin/hisnos-emergency "${reason}"
    # If exec fails (emergency not found), fall back to a hard panic.
    echo "HISNOS BOOT FATAL: ${reason}" >&2
    echo "HISNOS BOOT FATAL: ${reason}" > /dev/kmsg 2>/dev/null || true
    sleep 5
    echo b > /proc/sysrq-trigger 2>/dev/null || true
    exit 1
}

# ── Directory initialisation ───────────────────────────────────────────────
hisnos_init_dirs() {
    mkdir -p \
        "${HISNOS_RUN}" \
        "${HISNOS_ISO_MNT}" \
        "${HISNOS_SQUASHFS_MNT}" \
        "${HISNOS_OVERLAY_DIR}/upper" \
        "${HISNOS_OVERLAY_DIR}/work" \
        2>/dev/null || {
            echo "hisnos-live: FATAL: cannot create state directories" >&2
            exit 1
        }
    touch "${HISNOS_LOG}" 2>/dev/null || true
}

# ── Source device detection ───────────────────────────────────────────────
# Sets the global HISNOS_SOURCE_DEV on success; returns 1 on failure.
# Detection order (first match wins):
#   1. hisnos.iso.device=<path>  — explicit cmdline override
#   2. root=live:CDLABEL=<label> — GRUB-set live label
#   3. blkid by label HISNOS_CDLABEL
#   4. Brute-force scan of all block devices for LiveOS/rootfs.img
hisnos_find_source_dev() {
    local dev tmp_mnt

    # Strategy 1: explicit override.
    dev=$(getarg hisnos.iso.device= 2>/dev/null || true)
    if [[ -n "${dev}" && -b "${dev}" ]]; then
        hisnos_log INFO "source dev (cmdline override): ${dev}"
        HISNOS_SOURCE_DEV="${dev}"; return 0
    fi

    # Strategy 2: root=live:CDLABEL=<label>
    local root_arg
    root_arg=$(getarg root= 2>/dev/null || true)
    if [[ "${root_arg}" =~ ^live:CDLABEL=(.+)$ ]]; then
        local label="${BASH_REMATCH[1]}"
        dev=$(blkid -L "${label}" 2>/dev/null | head -1)
        if [[ -n "${dev}" && -b "${dev}" ]]; then
            hisnos_log INFO "source dev (CDLABEL=${label}): ${dev}"
            HISNOS_SOURCE_DEV="${dev}"; return 0
        fi
        hisnos_log WARN "CDLABEL=${label} not found yet"
    fi

    # Strategy 3: blkid by label.
    dev=$(blkid -t "LABEL=${HISNOS_CDLABEL}" -o device 2>/dev/null | head -1)
    if [[ -n "${dev}" && -b "${dev}" ]]; then
        hisnos_log INFO "source dev (blkid label=${HISNOS_CDLABEL}): ${dev}"
        HISNOS_SOURCE_DEV="${dev}"; return 0
    fi

    # Strategy 4: scan all block devices (slow, last resort).
    hisnos_log INFO "scanning all block devices for ${HISNOS_LIVE_IMG}..."
    while IFS= read -r dev; do
        [[ -b "${dev}" ]] || continue
        tmp_mnt=$(mktemp -d /run/hisnos/.probe-XXXXXXXX 2>/dev/null) || continue
        if mount -o ro,noatime "${dev}" "${tmp_mnt}" 2>/dev/null; then
            if [[ -f "${tmp_mnt}${HISNOS_LIVE_IMG}" ]]; then
                umount "${tmp_mnt}" 2>/dev/null; rmdir "${tmp_mnt}" 2>/dev/null
                hisnos_log INFO "source dev (scan): ${dev}"
                HISNOS_SOURCE_DEV="${dev}"; return 0
            fi
            umount "${tmp_mnt}" 2>/dev/null
        fi
        rmdir "${tmp_mnt}" 2>/dev/null
    done < <(lsblk -rno PATH 2>/dev/null)

    hisnos_log WARN "no source device found"
    return 1
}

# ── Root validation ────────────────────────────────────────────────────────
# Checks that the candidate new root contains a minimum viable OS tree.
hisnos_validate_root() {
    local root="${1:-${NEWROOT}}"
    local ok=0

    [[ -d "${root}/usr" ]]               || { hisnos_log ERROR "missing ${root}/usr";  ok=1; }
    [[ -d "${root}/etc" ]]               || { hisnos_log ERROR "missing ${root}/etc";  ok=1; }
    [[ -d "${root}/bin" || -L "${root}/bin" ]] \
                                         || { hisnos_log ERROR "missing ${root}/bin";  ok=1; }
    [[ -x "${root}/sbin/init" || -x "${root}/lib/systemd/systemd" || -L "${root}/init" ]] \
                                         || { hisnos_log ERROR "no init found in ${root}"; ok=1; }
    findmnt -n "${root}" &>/dev/null     || { hisnos_log ERROR "${root} not a mountpoint"; ok=1; }

    return "${ok}"
}

# Compute RAM-proportional size string for tmpfs (in MiB, e.g. "1024m").
hisnos_overlay_size() {
    local ram_kb
    ram_kb=$(awk '/MemTotal/{print $2}' /proc/meminfo 2>/dev/null || echo 2097152)
    local size_mb=$(( ram_kb / 2 / 1024 ))
    [[ ${size_mb} -lt 512  ]] && size_mb=512
    [[ ${size_mb} -gt 4096 ]] && size_mb=4096
    echo "${size_mb}m"
}
