#!/usr/bin/env bash
# dracut/95hisnos/hisnos-lib.sh
#
# Shared library for HisnOS dracut hooks.
# Source this file at the top of each hook: . /hisnos-lib.sh
#
# Provides:
#   - Colour terminal output (ANSI, TERM-safe)
#   - getarg_hisnos: safe kernel cmdline parser
#   - hisnos_log:    structured log to journald + tty
#   - hisnos_flag:   read/write /run/hisnos/ state flags
#   - panic_banner:  emergency red banner with reboot prompt

# ─── dracut helpers ──────────────────────────────────────────────────────────
. /lib/dracut-lib.sh 2>/dev/null || true

# ─── Terminal colours (ANSI, safe-degraded) ──────────────────────────────────
if tput colors &>/dev/null 2>&1 && [[ "$(tput colors)" -ge 8 ]]; then
    RESET=$'\033[0m'
    BOLD=$'\033[1m'
    DIM=$'\033[2m'
    CYAN=$'\033[1;36m'
    YELLOW=$'\033[1;33m'
    RED=$'\033[1;31m'
    GREEN=$'\033[1;32m'
    MAGENTA=$'\033[1;35m'
else
    RESET="" BOLD="" DIM="" CYAN="" YELLOW="" RED="" GREEN="" MAGENTA=""
fi

# ─── /run/hisnos state directory ─────────────────────────────────────────────
HISNOS_RUN="/run/hisnos"
mkdir -p "$HISNOS_RUN" 2>/dev/null || true

# ─── Logging ─────────────────────────────────────────────────────────────────
_hisnos_ts() { date -u '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || echo "?"; }

hisnos_log() {
    local level="$1"; shift
    local msg="$*"
    local ts; ts="$(_hisnos_ts)"

    # TTY output
    case "$level" in
        INFO)  echo -e "${CYAN}[hisnos ${ts}]${RESET} ${msg}" ;;
        WARN)  echo -e "${YELLOW}[hisnos ${ts}] ⚠${RESET} ${msg}" ;;
        ERROR) echo -e "${RED}[hisnos ${ts}] ✘${RESET} ${msg}" ;;
        OK)    echo -e "${GREEN}[hisnos ${ts}] ✔${RESET} ${msg}" ;;
        *)     echo "[hisnos ${ts}] ${msg}" ;;
    esac

    # Journal (best-effort)
    logger -t "hisnos-dracut" -p "user.${level,,}" "${msg}" 2>/dev/null || true
}

# ─── Kernel cmdline helpers ───────────────────────────────────────────────────
# hisnos_getarg KEY → prints value if "KEY=VALUE", prints "1" if bare "KEY",
# returns 1 if KEY not present.
hisnos_getarg() {
    local key="$1"
    # Use dracut's getarg if available
    if type getarg &>/dev/null 2>&1; then
        getarg "${key}" 2>/dev/null && return 0
        getargbool 0 "${key}" 2>/dev/null && echo "1" && return 0
        return 1
    fi
    # Fallback: parse /proc/cmdline directly
    local cmdline; cmdline="$(cat /proc/cmdline 2>/dev/null)"
    local val
    # KEY=VALUE
    if val="$(echo "$cmdline" | grep -oP "(?<=\b${key}=)[^ ]+" 2>/dev/null)"; then
        [[ -n "$val" ]] && echo "$val" && return 0
    fi
    # bare KEY
    if echo "$cmdline" | grep -qwP "${key}"; then
        echo "1" && return 0
    fi
    return 1
}

# ─── State flag helpers ───────────────────────────────────────────────────────
# hisnos_flag set NAME [VALUE]   — write /run/hisnos/NAME
# hisnos_flag get NAME           — cat /run/hisnos/NAME (returns 1 if missing)
# hisnos_flag isset NAME         — returns 0 if file exists, 1 otherwise
hisnos_flag() {
    local op="$1" name="$2" value="${3:-1}"
    local path="${HISNOS_RUN}/${name}"
    case "$op" in
        set)   echo "$value" > "$path" ;;
        get)   [[ -f "$path" ]] && cat "$path" || return 1 ;;
        isset) [[ -f "$path" ]] ;;
        clear) rm -f "$path" ;;
    esac
}

# ─── Emergency banner ─────────────────────────────────────────────────────────
panic_banner() {
    local reason="$*"
    echo ""
    echo -e "${RED}${BOLD}╔══════════════════════════════════════════════════════════╗${RESET}"
    echo -e "${RED}${BOLD}║                  !! BOOT FAILURE !!                     ║${RESET}"
    echo -e "${RED}${BOLD}╠══════════════════════════════════════════════════════════╣${RESET}"
    echo -e "${RED}${BOLD}║${RESET} ${reason}"
    echo -e "${RED}${BOLD}╠══════════════════════════════════════════════════════════╣${RESET}"
    echo -e "${RED}${BOLD}║${RESET} Boot with: hisnos.recovery=1 to enter recovery"
    echo -e "${RED}${BOLD}║${RESET} Or press ENTER to reboot"
    echo -e "${RED}${BOLD}╚══════════════════════════════════════════════════════════╝${RESET}"
    echo ""
    read -r -t 30 _ 2>/dev/null || true
    reboot -f 2>/dev/null || true
}

# ─── Boot health score helpers ─────────────────────────────────────────────────
# Reads and validates /etc/hisnos/boot-health.json (if the installed system
# is already present on disk — only relevant after first install).
hisnos_read_boot_score() {
    local state_path="${HISNOS_RUN}/new-root/var/lib/hisnos/boot-health.json"
    [[ -f "$state_path" ]] || echo "0" && return 0
    # Extract rolling_score from JSON (no jq in initramfs, use grep+awk)
    grep -oP '"rolling_score":\s*\K[\d.]+' "$state_path" 2>/dev/null | head -1 || echo "0"
}
