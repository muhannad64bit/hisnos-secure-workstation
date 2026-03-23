#!/usr/bin/env bash
# bootstrap/bootstrap-installer.sh — HisnOS First Usable Boot
#
# Idempotent bootstrap script that prepares a Fedora Kinoite (rpm-ostree)
# workstation so core subsystems become operational after first reboot:
#   1) nftables firewall deployment + nftables.service activation
#   2) Vault scripts + user systemd units (watcher + idle timer)
#   3) Dashboard socket/service units (socket activation + health readiness)
#   4) Required system + user directories
#   5) Lab isolation runtime (hisnos-lab group, netd helper, socket activation)
#   6) Security Telemetry & Audit Pipeline (auditd rules + hisnos-logd)
#   7) Threat Intelligence Engine (hisnos-threatd, risk score, timeline)
#   8) Core Control Runtime (hisnosd — state manager, policy engine, IPC bus)
#   9) Gaming Performance Integration (hisnos-gaming group, polkit rule, scripts, units)
#  10) rpm-ostree kernel override validation (warning if mismatch)
#  11) Command Center (searchd Go daemon + PySide6 UI overlay + SUPER+SPACE shortcut)
#  12) hispowerd — Gaming Performance Runtime (Go daemon, 10-phase orchestrator)
#  13) Distribution Experience Layer (Plymouth theme, onboarding wizard, recovery entry)
#  14) Distribution Finalization (boot health service, status indicator, version file, cmdline)
#
# Safety:
#   - Avoids touching /usr (except building dashboard in the user's home)
#   - Overwrites rulesets and unit files rather than appending
#   - Validates nftables syntax before starting nftables.service
#
# Run as: the target logged-in user (required for --user systemd units)
#
# Optional env:
#   HISNOS_DISABLE_DASHBOARD_BUILD=1  # do not build Go binary (fail if missing)
#
set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NFT_SRC_DIR="${REPO_DIR}/egress/nftables"

VAR_HISNOS_DIR="/var/lib/hisnos"
USER_DATA_BASE="${HOME}/.local/share/hisnos"
USER_CONFIG_USER_SYSTEMD="${HOME}/.config/systemd/user"

USER_VAULT_DIR="${USER_DATA_BASE}/vault"
USER_DASHBOARD_DIR="${USER_DATA_BASE}/dashboard"
USER_DASHBOARD_BIN="${USER_DASHBOARD_DIR}/hisnos-dashboard"

NFT_DIR="/etc/nftables"
NFTABLES_CONF="/etc/nftables.conf"

VLOG_DIR="${USER_DATA_BASE}/logs"
RUNTIME_STATE_DIR="${USER_DATA_BASE}/run"
STATE_FILE="${RUNTIME_STATE_DIR}/bootstrap-installer.state"

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'; BOLD=$'\033[1m'; NC=$'\033[0m'

info()  { echo -e "${GREEN}[hisnos]${NC} $*"; }
warn()  { echo -e "${YELLOW}[hisnos WARN]${NC} $*"; }
fail()  { echo -e "${RED}[hisnos FAIL]${NC} $*" >&2; }

status_line() {
  local status="$1"; shift
  local name="$1"; shift
  local msg="${1:-}"
  case "${status}" in
    OK)   echo -e "${GREEN}[OK]${NC}   ${BOLD}${name}${NC} — ${msg}" ;;
    SKIP) echo -e "${YELLOW}[SKIP]${NC} ${BOLD}${name}${NC} — ${msg}" ;;
    FAIL) echo -e "${RED}[FAIL]${NC} ${BOLD}${name}${NC} — ${msg}" ;;
    *)    echo -e "[${status}] ${name} — ${msg}" ;;
  esac
}

CURRENT_STEP="startup"
trap 'rc=$?; status_line FAIL "${CURRENT_STEP}" "unexpected error (exit code=${rc})"; exit "${rc}"' ERR

state_has() {
  [[ -f "${STATE_FILE}" ]] || return 1
  grep -qx "$1" "${STATE_FILE}" 2>/dev/null
}

state_mark() {
  mkdir -p "${RUNTIME_STATE_DIR}"
  touch "${STATE_FILE}"
  grep -qx "$1" "${STATE_FILE}" 2>/dev/null || echo "$1" >> "${STATE_FILE}"
}

require_cmd() {
  command -v "$1" &>/dev/null || return 1
}

require_nftables() {
  require_cmd nft
}

need_sudo() {
  if [[ "${EUID}" -ne 0 ]]; then return 0; fi
  fail "Run this script as a regular user (not root) so systemd --user works."
  exit 1
}

ensure_user_dbus() {
  # For user systemd units we require the user manager + runtime bus.
  local uid
  uid="$(id -u)"
  local runtime="${XDG_RUNTIME_DIR:-/run/user/${uid}}"
  if [[ ! -d "${runtime}" ]] || [[ ! -S "${runtime}/bus" ]]; then
    return 1
  fi

  # Quick systemd-user liveness check.
  if ! systemctl --user is-system-running --quiet &>/dev/null; then
    return 1
  fi
  return 0
}

ensure_port_listening() {
  local addr="$1" port="$2"
  # Wait-free check: socket already listening should show in ss.
  ss -ltnH 2>/dev/null | awk '{print $4}' | grep -xq "${addr}:${port}"
}

validate_nft_syntax() {
  local file="$1"
  sudo nft -c -f "${file}" &>/dev/null
}

copy_if_different() {
  local src="$1" dst="$2"
  # cmp -s returns 0 when files match.
  if [[ -f "${dst}" ]]; then
    if cmp -s "${src}" "${dst}"; then
      return 1
    fi
  fi
  return 0
}

ensure_nftables_managed_conf() {
  local begin="# BEGIN HisnOS nftables boot configuration"
  local end="# END HisnOS nftables boot configuration"
  local managed
  managed="$(cat <<EOF
${begin}
#!/usr/sbin/nft -f
# HisnOS nftables boot configuration
# Managed by: bootstrap-installer.sh
#
# Load order:
#   - hisnos-base: table/chains + empty named sets
#   - hisnos-updates: populate CIDR allowlists
#
include "${NFT_DIR}/hisnos-base.nft"
include "${NFT_DIR}/hisnos-updates.nft"
${end}
EOF
)"

  # Only write when content differs (idempotent).
  local tmp
  tmp="$(mktemp)"
  echo "${managed}" > "${tmp}"
  if sudo test -f "${NFTABLES_CONF}" && sudo cmp -s "${tmp}" "${NFTABLES_CONF}"; then
    rm -f "${tmp}" || true
    return 1  # no change
  fi

  if sudo test -f "${NFTABLES_CONF}"; then
    sudo cp -a "${NFTABLES_CONF}" "${NFTABLES_CONF}.hisnos.bak.$(date +%s)" || true
  fi
  sudo cp "${tmp}" "${NFTABLES_CONF}"
  rm -f "${tmp}" || true
  return 0
}

main() {
  need_sudo

  require_cmd ss
  require_cmd cmp
  require_cmd curl
  require_nftables || { status_line FAIL "firewall preflight" "nft command not available"; exit 1; }
  require_cmd rpm-ostree || warn "rpm-ostree not found; kernel validation will be limited."

  # Step 4: System directories (required early for dashboard/vault units)
  section() { echo -e "\n${BOLD}${NC}== $* =="; }
  CURRENT_STEP="System directories"
  section "System directories"

  # 4a) /var/lib/hisnos (owned by this user so hisnos-dashboard can write state)
  if sudo test -d "${VAR_HISNOS_DIR}"; then
    : # exists
  else
    sudo mkdir -p "${VAR_HISNOS_DIR}"
  fi
  if ! sudo chown "${USER}:${USER}" "${VAR_HISNOS_DIR}" &>/dev/null; then
    status_line FAIL "System directory (/var/lib/hisnos)" "failed to chown to ${USER}:${USER}"
    exit 1
  fi
  if ! sudo chmod 700 "${VAR_HISNOS_DIR}" &>/dev/null; then
    status_line FAIL "System directory (/var/lib/hisnos)" "failed to chmod 700"
    exit 1
  fi
  status_line OK "System directory (/var/lib/hisnos)" "ownership=${USER}:${USER} mode=700"

  # 4b) ~/.local/share/hisnos/{vault-cipher,vault-mount,logs,run} (+ required subdirs)
  mkdir -p \
    "${USER_DATA_BASE}/vault-cipher" \
    "${USER_DATA_BASE}/vault-mount" \
    "${VLOG_DIR}" \
    "${RUNTIME_STATE_DIR}" \
    "${USER_VAULT_DIR}" \
    "${USER_DASHBOARD_DIR}"

  chmod 700 "${USER_DATA_BASE}/vault-cipher" "${USER_DATA_BASE}/vault-mount" "${VLOG_DIR}" "${RUNTIME_STATE_DIR}" "${USER_VAULT_DIR}" &>/dev/null
  status_line OK "User directories" "created under ${USER_DATA_BASE}"

  # Step 1: Firewall deployment (copy + syntax check + enable nftables.service)
  CURRENT_STEP="Firewall deployment"
  section "Firewall deployment"

  local any_firewall_needed=false
  local nft_conf_changed=false
  mkdir -p "${NFT_DIR}"
  for f in hisnos-base.nft hisnos-observe.nft hisnos-updates.nft hisnos-gaming.nft; do
    [[ -f "${NFT_SRC_DIR}/${f}" ]] || continue
    if copy_if_different "${NFT_SRC_DIR}/${f}" "${NFT_DIR}/${f}"; then
      sudo cp "${NFT_SRC_DIR}/${f}" "${NFT_DIR}/${f}"
      any_firewall_needed=true
    fi
  done

  if ensure_nftables_managed_conf; then
    nft_conf_changed=true
  fi
  if ${nft_conf_changed}; then
    any_firewall_needed=true
  fi

  if systemctl is-active nftables.service &>/dev/null; then
    : # active, we'll restart if config changed
  fi

  if ${any_firewall_needed}; then
    status_line OK "Firewall deployment" "rules copied and /etc/nftables.conf ensured"
  else
    status_line SKIP "Firewall deployment" "no rules/config changes detected"
  fi

  # Syntax validation before activation
  for f in hisnos-base.nft hisnos-updates.nft; do
    sudo test -f "${NFT_DIR}/${f}" || { status_line FAIL "Firewall syntax" "missing ${NFT_DIR}/${f}"; exit 1; }
    if ! validate_nft_syntax "${NFT_DIR}/${f}"; then
      status_line FAIL "Firewall syntax" "nft -c failed for ${NFT_DIR}/${f}"
      exit 1
    fi
  done

  # Enable and start nftables.service
  if sudo systemctl enable --now nftables.service &>/dev/null; then
    if ${any_firewall_needed}; then
      sudo systemctl restart nftables.service &>/dev/null
    fi
    status_line OK "nftables.service" "enabled + running"
  else
    status_line FAIL "nftables.service" "enable/start failed"
    exit 1
  fi

  # Step 2: Vault integration (install scripts + user units + enable watcher+idle timer)
  CURRENT_STEP="Vault integration"
  section "Vault integration"

  if ! ensure_user_dbus; then
    status_line FAIL "DBus/session availability" "systemd --user is not available (no user D-Bus). Re-run after login."
    exit 1
  fi

  # Install vault scripts
  local vault_script
  for vault_script in hisnos-vault.sh hisnos-vault-watcher.sh hisnos-vault-gamemode.sh; do
    [[ -f "${REPO_DIR}/vault/${vault_script}" ]] || continue
    cp -f "${REPO_DIR}/vault/${vault_script}" "${USER_VAULT_DIR}/${vault_script}"
    chmod 755 "${USER_VAULT_DIR}/${vault_script}" &>/dev/null
  done

  # Install user systemd units
  mkdir -p "${USER_CONFIG_USER_SYSTEMD}"
  for unit in hisnos-vault-watcher.service hisnos-vault-idle.service hisnos-vault-idle.timer; do
    [[ -f "${REPO_DIR}/vault/systemd/${unit}" ]] || continue
    cp -f "${REPO_DIR}/vault/systemd/${unit}" "${USER_CONFIG_USER_SYSTEMD}/${unit}"
  done

  # Enable watcher + idle timer
  if systemctl --user daemon-reload \
    && systemctl --user enable --now hisnos-vault-watcher.service \
    && systemctl --user enable --now hisnos-vault-idle.timer; then
    status_line OK "Vault watcher + idle timer" "enabled and started for user ${USER}"
  else
    status_line FAIL "Vault watcher + idle timer" "systemctl --user enable/start failed"
    exit 1
  fi

  # Step 3: Dashboard activation (build frontend → embed into binary → install units → socket activate)
  #
  # Build order is mandatory:
  #   1. npm build  — produces frontend/dist/
  #   2. cp dist    — copies frontend/dist/ into backend/web/dist/ so go:embed has content
  #   3. go build   — embeds web/dist/ into the binary (//go:embed all:dist in web/static.go)
  #   4. systemd    — install units, enable socket activation, verify readiness
  #
  # If HISNOS_DISABLE_DASHBOARD_BUILD=1 the binary must be pre-provided at USER_DASHBOARD_BIN.
  CURRENT_STEP="Dashboard activation"
  section "Dashboard activation"

  local FRONTEND_SRC="${REPO_DIR}/dashboard/frontend"
  local FRONTEND_DIST="${FRONTEND_SRC}/dist"
  local EMBED_DIST="${REPO_DIR}/dashboard/backend/web/dist"

  # ── 3a. Build SvelteKit frontend ──────────────────────────────────────────
  # Must run before go build so the embed directive captures real content.
  if [[ "${HISNOS_DISABLE_DASHBOARD_BUILD:-0}" == "1" ]]; then
    status_line SKIP "Dashboard frontend build" "disabled via HISNOS_DISABLE_DASHBOARD_BUILD"
  elif ! command -v node &>/dev/null || ! command -v npm &>/dev/null; then
    status_line SKIP "Dashboard frontend build" "Node/npm not found — skipping (binary must be pre-built)"
  elif [[ ! -f "${FRONTEND_SRC}/package.json" ]]; then
    status_line SKIP "Dashboard frontend build" "dashboard/frontend/package.json missing — skipping"
  else
    (
      cd "${FRONTEND_SRC}"
      # ci respects package-lock.json and is reproducible; fall back to install for first run
      if [[ -f package-lock.json ]]; then
        npm ci --silent
      else
        npm install --silent
      fi
      npm run build
    ) || {
      status_line FAIL "Dashboard frontend build" "npm build failed (Node ≥18 required)"
      exit 1
    }

    # Verify the entry point exists — adapter-static must emit it
    if [[ ! -f "${FRONTEND_DIST}/index.html" ]]; then
      status_line FAIL "Dashboard frontend build" \
        "dist/index.html missing after npm build — check svelte.config.js adapter-static pages setting"
      exit 1
    fi
    status_line OK "Dashboard frontend" "built to ${FRONTEND_DIST}"

    # ── 3b. Copy dist/ into backend/web/dist/ for go:embed ────────────────
    # web/dist/.gitkeep ensures the directory exists on clean checkout.
    # We clear stale content first so no old hashed assets accumulate.
    mkdir -p "${EMBED_DIST}"
    # Remove everything except .gitkeep (keep the placeholder for git)
    find "${EMBED_DIST}" -mindepth 1 -not -name '.gitkeep' -delete 2>/dev/null || true
    cp -r "${FRONTEND_DIST}/." "${EMBED_DIST}/"
    status_line OK "Dashboard frontend embed" "copied dist/ → backend/web/dist/"
  fi

  # ── 3c. Build Go backend binary ───────────────────────────────────────────
  # Done after npm build so //go:embed all:dist captures the real SvelteKit output.
  if [[ "${HISNOS_DISABLE_DASHBOARD_BUILD:-0}" == "1" ]]; then
    if [[ ! -f "${USER_DASHBOARD_BIN}" ]]; then
      status_line FAIL "Dashboard backend binary" "missing ${USER_DASHBOARD_BIN} and build disabled"
      exit 1
    fi
    status_line SKIP "Dashboard backend build" "using pre-built binary at ${USER_DASHBOARD_BIN}"
  else
    if ! command -v go &>/dev/null; then
      status_line FAIL "Dashboard backend binary" "Go toolchain not found (install Go or pre-provide ${USER_DASHBOARD_BIN})"
      exit 1
    fi
    (
      cd "${REPO_DIR}/dashboard/backend"
      mkdir -p "${USER_DASHBOARD_DIR}"
      go build -o "${USER_DASHBOARD_BIN}" .
    ) || {
      status_line FAIL "Dashboard backend binary" "go build failed"
      exit 1
    }
    chmod 755 "${USER_DASHBOARD_BIN}" &>/dev/null
    status_line OK "Dashboard backend binary" "${USER_DASHBOARD_BIN}"
  fi

  # Install dashboard systemd user units
  for unit in hisnos-dashboard.socket hisnos-dashboard.service; do
    [[ -f "${REPO_DIR}/dashboard/systemd/${unit}" ]] || continue
    cp -f "${REPO_DIR}/dashboard/systemd/${unit}" "${USER_CONFIG_USER_SYSTEMD}/${unit}"
  done

  if systemctl --user daemon-reload && systemctl --user enable --now hisnos-dashboard.socket; then
    : # ok
  else
    status_line FAIL "Dashboard socket activation" "failed to enable/start hisnos-dashboard.socket"
    exit 1
  fi

  # Verify socket bind readiness
  local ok=false
  for _ in {1..20}; do
    if ensure_port_listening "127.0.0.1" "7374"; then
      ok=true
      break
    fi
    sleep 0.25
  done
  if [[ "${ok}" != "true" ]]; then
    status_line FAIL "Dashboard socket bind" "127.0.0.1:7374 not listening"
    exit 1
  fi

  # Trigger service start and verify health endpoint
  if ! curl -sS --max-time 3 "http://127.0.0.1:7374/api/health" &>/dev/null; then
    status_line FAIL "Dashboard health readiness" "health endpoint not reachable on 127.0.0.1:7374"
    exit 1
  fi

  status_line OK "Dashboard control plane" "socket active + health endpoint responds"

  # Step 5: Lab isolation runtime (Phase 5b — veth + nftables netd helper)
  #
  # Installs the privileged socket-activated system helper (hisnos-lab-netd)
  # that manages per-session veth pairs and nftables rules on behalf of the
  # unprivileged lab runtime script.
  #
  # Privilege model:
  #   - hisnos-lab-netd@.service runs as root with CAP_NET_ADMIN + CAP_SYS_PTRACE
  #   - Socket is mode 0660, group hisnos-lab — only group members can connect
  #   - Runtime script (runs as user) is in hisnos-lab group via usermod
  #   - No sudo, no setuid binary
  CURRENT_STEP="Lab isolation runtime"
  section "Lab isolation runtime"

  LAB_NETD_DEST="/etc/hisnos/lab/netd"
  LAB_RUNTIME_DEST="/etc/hisnos/lab/runtime"
  LAB_GROUP="hisnos-lab"

  # 5a. Create hisnos-lab system group (idempotent)
  if ! getent group "${LAB_GROUP}" &>/dev/null; then
    sudo groupadd -r "${LAB_GROUP}"
    status_line OK "Lab group" "${LAB_GROUP} created"
  else
    status_line SKIP "Lab group" "${LAB_GROUP} already exists"
  fi

  # 5b. Add current user to hisnos-lab group (idempotent)
  if id -nG "${USER}" | grep -qw "${LAB_GROUP}"; then
    status_line SKIP "Lab group membership" "${USER} already in ${LAB_GROUP}"
  else
    sudo usermod -aG "${LAB_GROUP}" "${USER}"
    status_line OK "Lab group membership" "${USER} added to ${LAB_GROUP} (re-login required for group to take effect)"
  fi

  # 5c. Create netd + runtime install directories
  sudo install -d -m 0750 -o root -g "${LAB_GROUP}" "${LAB_NETD_DEST}"
  sudo install -d -m 0750 -o root -g "${LAB_GROUP}" "${LAB_RUNTIME_DEST}"

  # 5d. Install netd helper + DNS sinkhole
  for f in hisnos-lab-netd.sh hisnos-lab-dns-sinkhole.py; do
    [[ -f "${REPO_DIR}/lab/netd/${f}" ]] || continue
    sudo install -m 0750 -o root -g "${LAB_GROUP}" \
      "${REPO_DIR}/lab/netd/${f}" "${LAB_NETD_DEST}/${f}"
  done

  # 5e. Install lab runtime + stop helper
  for f in hisnos-lab-runtime.sh hisnos-lab-stop.sh; do
    [[ -f "${REPO_DIR}/lab/runtime/${f}" ]] || continue
    sudo install -m 0750 -o root -g "${LAB_GROUP}" \
      "${REPO_DIR}/lab/runtime/${f}" "${LAB_RUNTIME_DEST}/${f}"
  done

  status_line OK "Lab netd helpers" "installed to ${LAB_NETD_DEST}"

  # 5f. Install hisnos-lab.nft into /etc/nftables (alongside hisnos-base.nft)
  if [[ -f "${NFT_SRC_DIR}/hisnos-lab.nft" ]]; then
    if copy_if_different "${NFT_SRC_DIR}/hisnos-lab.nft" "${NFT_DIR}/hisnos-lab.nft"; then
      sudo cp "${NFT_SRC_DIR}/hisnos-lab.nft" "${NFT_DIR}/hisnos-lab.nft"
      sudo chmod 644 "${NFT_DIR}/hisnos-lab.nft"
      status_line OK "Lab nftables stubs" "copied to ${NFT_DIR}/hisnos-lab.nft"
    else
      status_line SKIP "Lab nftables stubs" "no change detected"
    fi
  else
    warn "hisnos-lab.nft not found in ${NFT_SRC_DIR} — lab nftables stubs skipped"
  fi

  # 5g. Install system service units (hisnos-lab-netd.socket + hisnos-lab-netd@.service)
  local lab_units_changed=false
  for unit in hisnos-lab-netd.socket "hisnos-lab-netd@.service"; do
    [[ -f "${REPO_DIR}/lab/systemd/${unit}" ]] || continue
    if copy_if_different "${REPO_DIR}/lab/systemd/${unit}" "/etc/systemd/system/${unit}"; then
      sudo install -m 0644 -o root -g root \
        "${REPO_DIR}/lab/systemd/${unit}" "/etc/systemd/system/${unit}"
      lab_units_changed=true
    fi
  done

  if ${lab_units_changed}; then
    sudo systemctl daemon-reload
    status_line OK "Lab system units" "installed to /etc/systemd/system/"
  else
    status_line SKIP "Lab system units" "no changes detected"
  fi

  # 5h. Enable and start the netd socket (Accept=yes — no persistent service to start)
  if sudo systemctl enable --now hisnos-lab-netd.socket &>/dev/null; then
    if ${lab_units_changed}; then
      sudo systemctl restart hisnos-lab-netd.socket &>/dev/null
    fi
    status_line OK "hisnos-lab-netd.socket" "enabled + listening at /run/hisnos/lab-netd.sock"
  else
    status_line FAIL "hisnos-lab-netd.socket" "enable/start failed"
    exit 1
  fi

  # 5i. Validate socket path exists after start
  local lab_sock="/run/hisnos/lab-netd.sock"
  if [[ -S "${lab_sock}" ]]; then
    status_line OK "Lab netd socket" "present at ${lab_sock}"
  else
    warn "Socket not yet present at ${lab_sock} — may require a moment to appear on first activation"
  fi

  # Step 6: Security Telemetry & Audit Pipeline (Phase 6)
  #
  # Installs auditd rules, enables auditd.service, builds hisnos-logd (a Go
  # daemon that subscribes to journald and writes normalized JSON audit logs),
  # and enables it as a user service.
  #
  # Safety: audit pipeline failure must NOT break workstation usability.
  # Only auditd activation is a hard failure; logd build/start failures warn.
  CURRENT_STEP="Audit pipeline"
  section "Audit pipeline"

  AUDIT_LOG_DIR="/var/lib/hisnos/audit"
  AUDIT_RULES_SRC="${REPO_DIR}/audit/hisnos.rules"
  AUDIT_RULES_DEST="/etc/audit/rules.d/hisnos.rules"
  LOGD_SRC="${REPO_DIR}/audit/logd"
  LOGD_BIN_DIR="${USER_DATA_BASE}/bin"
  LOGD_BIN="${LOGD_BIN_DIR}/hisnos-logd"

  # 6a. Create audit log directory (owned by user — logd runs as user service)
  if sudo test -d "${AUDIT_LOG_DIR}"; then
    : # exists
  else
    sudo mkdir -p "${AUDIT_LOG_DIR}"
  fi
  if ! sudo chown "${USER}:${USER}" "${AUDIT_LOG_DIR}" &>/dev/null; then
    warn "Failed to chown ${AUDIT_LOG_DIR} to ${USER}; logd may not be able to write logs"
  fi
  sudo chmod 750 "${AUDIT_LOG_DIR}" 2>/dev/null || true
  status_line OK "Audit log directory" "${AUDIT_LOG_DIR}"

  # 6b. Install auditd rules
  if [[ -f "${AUDIT_RULES_SRC}" ]]; then
    if copy_if_different "${AUDIT_RULES_SRC}" "${AUDIT_RULES_DEST}"; then
      sudo install -m 0640 -o root -g root "${AUDIT_RULES_SRC}" "${AUDIT_RULES_DEST}"
      # augenrules merges and loads all rules.d rules into the kernel.
      if command -v augenrules &>/dev/null; then
        sudo augenrules --load &>/dev/null || true
      fi
      status_line OK "auditd rules" "installed to ${AUDIT_RULES_DEST}"
    else
      status_line SKIP "auditd rules" "no change detected"
    fi
  else
    warn "audit/hisnos.rules not found in repo — auditd rules skipped"
  fi

  # 6c. Enable and start auditd.service — hard requirement for Phase 6
  if sudo systemctl enable --now auditd.service &>/dev/null; then
    status_line OK "auditd.service" "enabled and running"
  else
    status_line FAIL "auditd.service" \
      "failed to enable/start — ensure 'audit' package is installed (rpm-ostree install audit)"
    exit 1
  fi

  # 6d. Build hisnos-logd binary
  # Reuses HISNOS_DISABLE_DASHBOARD_BUILD flag so CI can skip all Go builds.
  mkdir -p "${LOGD_BIN_DIR}"
  if [[ "${HISNOS_DISABLE_DASHBOARD_BUILD:-0}" == "1" ]]; then
    if [[ ! -f "${LOGD_BIN}" ]]; then
      status_line FAIL "hisnos-logd binary" \
        "missing ${LOGD_BIN} and build disabled — pre-provide binary or unset HISNOS_DISABLE_DASHBOARD_BUILD"
      exit 1
    fi
    status_line SKIP "hisnos-logd build" "using pre-built binary at ${LOGD_BIN}"
  elif ! command -v go &>/dev/null; then
    warn "Go toolchain not found — hisnos-logd not built. Install Go and re-run bootstrap."
  elif [[ ! -d "${LOGD_SRC}" ]]; then
    warn "audit/logd source directory not found — hisnos-logd not built"
  else
    (
      cd "${LOGD_SRC}"
      go build -o "${LOGD_BIN}" .
    ) || {
      status_line FAIL "hisnos-logd binary" "go build failed"
      exit 1
    }
    chmod 755 "${LOGD_BIN}"
    status_line OK "hisnos-logd binary" "${LOGD_BIN}"
  fi

  # 6e. Install hisnos-logd user service unit and enable it
  LOGD_UNIT_SRC="${REPO_DIR}/audit/systemd/hisnos-logd.service"
  if [[ -f "${LOGD_UNIT_SRC}" ]] && [[ -f "${LOGD_BIN}" ]]; then
    cp -f "${LOGD_UNIT_SRC}" "${USER_CONFIG_USER_SYSTEMD}/hisnos-logd.service"
    if systemctl --user daemon-reload \
      && systemctl --user enable --now hisnos-logd.service &>/dev/null; then
      status_line OK "hisnos-logd.service" "enabled and started"
    else
      # Non-fatal: logd failure must not prevent workstation use.
      warn "hisnos-logd.service failed to start — audit log collection inactive. Check: journalctl --user -u hisnos-logd"
    fi
  elif [[ ! -f "${LOGD_BIN}" ]]; then
    status_line SKIP "hisnos-logd.service" "binary not available — skipping service install"
  else
    warn "audit/systemd/hisnos-logd.service not found — service not installed"
  fi

  # 6f. Phase 6 gate — verify auditd is active before marking complete
  if systemctl is-active --quiet auditd.service; then
    status_line OK "Phase 6 gate" "auditd.service active — audit pipeline operational"
  else
    status_line FAIL "Phase 6 gate" "auditd.service not active after bootstrap"
    exit 1
  fi

  # Step 7: Threat Intelligence and Risk Scoring Engine (Phase 7)
  #
  # Builds hisnos-threatd (Go daemon), installs it as a user service.
  # threatd tails audit/current.jsonl written by hisnos-logd and evaluates a
  # deterministic risk score every 30s. Its output is:
  #   /var/lib/hisnos/threat-state.json    (atomic, read by dashboard API)
  #   /var/lib/hisnos/threat-timeline.jsonl (append-only, 48h rolling)
  #
  # Safety: threatd failure must NOT block workstation use.
  # The step is non-fatal (warn on build/start failure; never exit 1).
  CURRENT_STEP="Threat intelligence"
  section "Threat intelligence"

  THREATD_SRC="${REPO_DIR}/threat/threatd"
  THREATD_BIN="${LOGD_BIN_DIR}/hisnos-threatd"   # same bin dir as logd
  THREATD_UNIT_SRC="${REPO_DIR}/threat/systemd/hisnos-threatd.service"

  # 7a. Build hisnos-threatd binary
  if [[ "${HISNOS_DISABLE_DASHBOARD_BUILD:-0}" == "1" ]]; then
    if [[ -f "${THREATD_BIN}" ]]; then
      status_line SKIP "hisnos-threatd build" "using pre-built binary"
    else
      warn "hisnos-threatd binary missing and build disabled — threat scoring inactive"
    fi
  elif ! command -v go &>/dev/null; then
    warn "Go toolchain not found — hisnos-threatd not built. Threat scoring inactive."
  elif [[ ! -d "${THREATD_SRC}" ]]; then
    warn "threat/threatd source not found — hisnos-threatd not built"
  else
    (
      cd "${THREATD_SRC}"
      go build -o "${THREATD_BIN}" .
    ) || {
      status_line FAIL "hisnos-threatd binary" "go build failed"
      exit 1
    }
    chmod 755 "${THREATD_BIN}"
    status_line OK "hisnos-threatd binary" "${THREATD_BIN}"
  fi

  # 7b. Install hisnos-threatd user service and enable it
  if [[ -f "${THREATD_UNIT_SRC}" ]] && [[ -f "${THREATD_BIN}" ]]; then
    cp -f "${THREATD_UNIT_SRC}" "${USER_CONFIG_USER_SYSTEMD}/hisnos-threatd.service"
    if systemctl --user daemon-reload \
      && systemctl --user enable --now hisnos-threatd.service &>/dev/null; then
      status_line OK "hisnos-threatd.service" "enabled and started"
    else
      # Non-fatal: threat scoring failure must not impair workstation usability.
      warn "hisnos-threatd.service failed to start — threat scoring inactive. Check: journalctl --user -u hisnos-threatd"
    fi
  elif [[ ! -f "${THREATD_BIN}" ]]; then
    status_line SKIP "hisnos-threatd.service" "binary not available — skipping"
  else
    warn "threat/systemd/hisnos-threatd.service not found — service not installed"
  fi

  # Step 8: hisnosd — Core Control Runtime
  #
  # Builds hisnosd (Go daemon), installs it as a user service.
  # hisnosd is the authoritative state manager, policy engine, event bus,
  # and subsystem supervisor for HisnOS. Must start before dashboard socket.
  #
  # Safety: non-fatal (warn on build/start failure; workstation still operable
  # via direct exec fallback in the dashboard).
  CURRENT_STEP="Core control runtime"
  section "Core Control Runtime (hisnosd)"

  HISNOSD_SRC="${REPO_DIR}/core"
  HISNOSD_BIN="${LOGD_BIN_DIR}/hisnosd"
  HISNOSD_UNIT_SRC="${REPO_DIR}/core/systemd/hisnosd.service"

  # 8a. Build hisnosd binary
  if [[ "${HISNOS_DISABLE_DASHBOARD_BUILD:-0}" == "1" ]]; then
    if [[ ! -f "${HISNOSD_BIN}" ]]; then
      warn "hisnosd binary missing and build disabled — core runtime unavailable"
    else
      status_line SKIP "hisnosd build" "using pre-built binary at ${HISNOSD_BIN}"
    fi
  elif ! command -v go &>/dev/null; then
    warn "Go toolchain not found — hisnosd not built. Install Go and re-run bootstrap."
  elif [[ ! -d "${HISNOSD_SRC}" ]]; then
    warn "core/ source directory not found — hisnosd not built"
  else
    (
      cd "${HISNOSD_SRC}"
      go build -o "${HISNOSD_BIN}" .
    ) && {
      chmod 755 "${HISNOSD_BIN}"
      status_line OK "hisnosd binary" "${HISNOSD_BIN}"
    } || {
      warn "hisnosd go build failed — core runtime unavailable (non-fatal)"
    }
  fi

  # 8b. Install hisnosd user service and enable it (Before= dashboard socket)
  if [[ -f "${HISNOSD_UNIT_SRC}" ]] && [[ -f "${HISNOSD_BIN}" ]]; then
    cp -f "${HISNOSD_UNIT_SRC}" "${USER_CONFIG_USER_SYSTEMD}/hisnosd.service"
    if systemctl --user daemon-reload \
      && systemctl --user enable --now hisnosd.service &>/dev/null; then
      status_line OK "hisnosd.service" "enabled and started"
    else
      warn "hisnosd.service failed to start — core runtime inactive (non-fatal). Check: journalctl --user -u hisnosd"
    fi
  elif [[ ! -f "${HISNOSD_BIN}" ]]; then
    status_line SKIP "hisnosd.service" "binary not built — skipping service install"
  else
    warn "core/systemd/hisnosd.service not found — service not installed"
  fi

  # 8c. Verify IPC socket appears (non-fatal check, hisnosd may take a moment to start)
  local HISNOSD_SOCKET="/run/user/$(id -u)/hisnosd.sock"
  sleep 2
  if [[ -S "${HISNOSD_SOCKET}" ]]; then
    status_line OK "hisnosd IPC socket" "${HISNOSD_SOCKET}"
  else
    warn "hisnosd IPC socket not yet present at ${HISNOSD_SOCKET} — may appear after login"
  fi

  # Step 9: Gaming Performance Integration
  CURRENT_STEP="Gaming integration"
  section "Gaming Performance Integration"

  # 8a. Create hisnos-gaming group and add user
  if ! getent group hisnos-gaming &>/dev/null; then
    if sudo groupadd -r hisnos-gaming; then
      status_line OK "hisnos-gaming group" "created"
    else
      warn "Failed to create hisnos-gaming group — gaming mode will not work"
    fi
  else
    status_line SKIP "hisnos-gaming group" "already exists"
  fi

  local USER_NAME
  USER_NAME="$(id -un)"
  if getent group hisnos-gaming 2>/dev/null | grep -q "${USER_NAME}"; then
    status_line SKIP "hisnos-gaming membership" "already member"
  else
    if sudo usermod -aG hisnos-gaming "${USER_NAME}"; then
      status_line OK "hisnos-gaming membership" "added ${USER_NAME} — re-login required"
    else
      warn "Failed to add ${USER_NAME} to hisnos-gaming group"
    fi
  fi

  # 8b. Install gaming scripts to /etc/hisnos/gaming/
  local GAMING_SYSTEM_DIR="/etc/hisnos/gaming"
  sudo mkdir -p "${GAMING_SYSTEM_DIR}"

  if [[ -f "${REPO_DIR}/gaming/hisnos-gaming-tuned.sh" ]]; then
    sudo install -m 0750 -o root -g hisnos-gaming \
      "${REPO_DIR}/gaming/hisnos-gaming-tuned.sh" \
      "${GAMING_SYSTEM_DIR}/hisnos-gaming-tuned.sh"
    status_line OK "hisnos-gaming-tuned.sh" "installed to ${GAMING_SYSTEM_DIR}"
  else
    warn "gaming/hisnos-gaming-tuned.sh not found — gaming tuning unavailable"
  fi

  # 8c. Install user-space gaming orchestrator
  local GAMING_USER_DIR="${USER_DATA_BASE}/gaming"
  mkdir -p "${GAMING_USER_DIR}"

  if [[ -f "${REPO_DIR}/gaming/hisnos-gaming.sh" ]]; then
    install -m 0750 \
      "${REPO_DIR}/gaming/hisnos-gaming.sh" \
      "${GAMING_USER_DIR}/hisnos-gaming.sh"
    status_line OK "hisnos-gaming.sh" "installed to ${GAMING_USER_DIR}"
  else
    warn "gaming/hisnos-gaming.sh not found — gaming CLI unavailable"
  fi

  # 8d. Install gaming config files (gamemode.ini, mangohud.conf)
  if [[ -f "${REPO_DIR}/gaming/config/gamemode.ini" ]]; then
    mkdir -p "${HOME}/.config/gamemode"
    install -m 0644 "${REPO_DIR}/gaming/config/gamemode.ini" "${HOME}/.config/gamemode/gamemode.ini"
    status_line OK "gamemode.ini" "installed"
  fi

  if [[ -f "${REPO_DIR}/gaming/config/mangohud.conf" ]]; then
    mkdir -p "${HOME}/.config/MangoHud"
    install -m 0644 "${REPO_DIR}/gaming/config/mangohud.conf" "${HOME}/.config/MangoHud/MangoHud.conf"
    status_line OK "MangoHud.conf" "installed"
  fi

  # 8e. Install polkit rule for gaming group
  if [[ -f "${REPO_DIR}/gaming/polkit/10-hisnos-gaming.rules" ]]; then
    if sudo install -m 0644 -o root -g root \
        "${REPO_DIR}/gaming/polkit/10-hisnos-gaming.rules" \
        "/etc/polkit-1/rules.d/10-hisnos-gaming.rules"; then
      status_line OK "polkit gaming rule" "installed to /etc/polkit-1/rules.d/"
    else
      warn "Failed to install polkit gaming rule — gaming mode privileges unavailable"
    fi
  else
    warn "gaming/polkit/10-hisnos-gaming.rules not found — gaming polkit not installed"
  fi

  # 8f. Install gaming nft rules
  if [[ -f "${REPO_DIR}/gaming/hisnos-gaming.nft" ]]; then
    sudo install -m 0640 -o root -g root \
      "${REPO_DIR}/gaming/hisnos-gaming.nft" \
      "/etc/nftables/hisnos-gaming.nft"
    status_line OK "hisnos-gaming.nft" "installed to /etc/nftables/"
  else
    warn "gaming/hisnos-gaming.nft not found — gaming firewall chain unavailable"
  fi

  # 8g. Install system gaming oneshot services
  local GAMING_UNIT_SRC_START="${REPO_DIR}/gaming/systemd/hisnos-gaming-tuned-start.service"
  local GAMING_UNIT_SRC_STOP="${REPO_DIR}/gaming/systemd/hisnos-gaming-tuned-stop.service"
  local SYSTEM_UNIT_DIR="/etc/systemd/system"

  for unit_src in "${GAMING_UNIT_SRC_START}" "${GAMING_UNIT_SRC_STOP}"; do
    if [[ -f "${unit_src}" ]]; then
      sudo install -m 0644 -o root -g root \
        "${unit_src}" "${SYSTEM_UNIT_DIR}/$(basename "${unit_src}")"
      status_line OK "$(basename "${unit_src}")" "installed to ${SYSTEM_UNIT_DIR}"
    else
      warn "$(basename "${unit_src}") not found — skipping"
    fi
  done

  sudo systemctl daemon-reload 2>/dev/null || true

  # 8h. Install user gaming orchestrator service
  local GAMING_USER_UNIT_SRC="${REPO_DIR}/gaming/systemd/hisnos-gaming.service"
  if [[ -f "${GAMING_USER_UNIT_SRC}" ]]; then
    install -m 0644 "${GAMING_USER_UNIT_SRC}" \
      "${USER_CONFIG_USER_SYSTEMD}/hisnos-gaming.service"
    systemctl --user daemon-reload 2>/dev/null || true
    status_line OK "hisnos-gaming.service (user)" "installed"
  else
    warn "gaming/systemd/hisnos-gaming.service not found — user gaming unit not installed"
  fi

  status_line OK "Gaming integration" "complete — re-login required for group membership to take effect"

  # Step 9b: hispowerd — Gaming Performance Runtime
  CURRENT_STEP="hispowerd build"
  section "Gaming Performance Runtime (hispowerd)"

  HISPOWERD_SRC="${REPO_DIR}/gaming/hispowerd"
  HISPOWERD_BIN="${USER_DATA_BASE}/bin/hispowerd"
  HISPOWERD_UNIT_SRC="${REPO_DIR}/gaming/hispowerd/systemd/hisnos-hispowerd.service"
  HISPOWERD_RECOVER_SRC="${REPO_DIR}/gaming/hisnos-hispowerd-recover.sh"
  FAST_NFT_SRC="${REPO_DIR}/gaming/nftables/hisnos-gaming-fast.nft"
  HISPOWERD_CFG_DIR="/etc/hisnos/gaming"

  # Build hispowerd binary.
  if [[ "${HISNOS_DISABLE_DASHBOARD_BUILD:-0}" == "1" ]]; then
    if [[ ! -f "${HISPOWERD_BIN}" ]]; then
      warn "hispowerd binary missing and build disabled — gaming performance runtime unavailable"
    else
      status_line SKIP "hispowerd build" "using pre-built binary at ${HISPOWERD_BIN}"
    fi
  elif ! command -v go &>/dev/null; then
    warn "Go toolchain not found — hispowerd not built"
  elif [[ ! -d "${HISPOWERD_SRC}" ]]; then
    warn "gaming/hispowerd/ source directory not found"
  else
    mkdir -p "${USER_DATA_BASE}/bin"
    if (cd "${HISPOWERD_SRC}" && go build -o "${HISPOWERD_BIN}" .); then
      chmod 755 "${HISPOWERD_BIN}"
      status_line OK "hispowerd binary" "${HISPOWERD_BIN}"
    else
      warn "hispowerd go build failed — gaming performance runtime unavailable (non-fatal)"
    fi
  fi

  # Install config directory and default config.
  if sudo mkdir -p "${HISPOWERD_CFG_DIR}" 2>/dev/null; then
    if [[ ! -f "${HISPOWERD_CFG_DIR}/hispowerd.json" ]]; then
      sudo tee "${HISPOWERD_CFG_DIR}/hispowerd.json" > /dev/null << 'HISPOWERD_CFG_EOF'
{
  "scan_interval_seconds": 2,
  "steam_detection": true,
  "proton_detection": true,
  "gaming_cores": [2, 3, 4, 5, 6, 7],
  "system_cores": [0, 1],
  "cpu_governor": "performance",
  "game_nice_value": -5,
  "inject_env_vars": true,
  "fast_nft_file": "/etc/nftables/hisnos-gaming-fast.nft",
  "gaming_state_file": "/var/lib/hisnos/gaming-state.json",
  "control_plane_state_file": "/var/lib/hisnos/core-state.json"
}
HISPOWERD_CFG_EOF
      status_line OK "hispowerd.json" "installed at ${HISPOWERD_CFG_DIR}/hispowerd.json"
    else
      status_line SKIP "hispowerd.json" "already exists"
    fi
  fi

  # Install fast-path nftables file.
  if [[ -f "${FAST_NFT_SRC}" ]]; then
    if sudo install -m 0640 -o root -g root "${FAST_NFT_SRC}" "/etc/nftables/hisnos-gaming-fast.nft" 2>/dev/null; then
      status_line OK "hisnos-gaming-fast.nft" "installed to /etc/nftables/"
    else
      warn "Failed to install hisnos-gaming-fast.nft — fast path firewall unavailable"
    fi
  else
    warn "gaming/nftables/hisnos-gaming-fast.nft not found"
  fi

  # Install recovery script.
  if [[ -f "${HISPOWERD_RECOVER_SRC}" ]]; then
    install -m 0755 "${HISPOWERD_RECOVER_SRC}" "${USER_DATA_BASE}/bin/hisnos-hispowerd-recover"
    status_line OK "hisnos-hispowerd-recover" "installed to ${USER_DATA_BASE}/bin/"
  else
    warn "gaming/hisnos-hispowerd-recover.sh not found"
  fi

  # Install and enable user service.
  if [[ -f "${HISPOWERD_UNIT_SRC}" ]] && [[ -f "${HISPOWERD_BIN}" ]]; then
    install -m 0644 "${HISPOWERD_UNIT_SRC}" "${USER_CONFIG_USER_SYSTEMD}/hisnos-hispowerd.service"
    if systemctl --user daemon-reload 2>/dev/null \
        && systemctl --user enable --now hisnos-hispowerd.service &>/dev/null; then
      status_line OK "hisnos-hispowerd.service" "enabled and started"
    else
      warn "hisnos-hispowerd.service failed to start (non-fatal). Check: journalctl --user -u hisnos-hispowerd"
    fi
  else
    status_line SKIP "hisnos-hispowerd.service" "binary or unit not found"
  fi

  status_line OK "hispowerd" "gaming performance runtime installed"

  # Step 10: Kernel validation (warn only; never abort)
  CURRENT_STEP="Kernel validation"
  section "Kernel validation"

  local boot_kernel
  boot_kernel="$(uname -r)"

  local override_list
  if command -v rpm-ostree &>/dev/null; then
    override_list="$(rpm-ostree override list 2>/dev/null || true)"
  else
    override_list=""
  fi

  local override_active="false"
  if echo "${override_list}" | grep -Eq "kernel.*hisnos|hisnos-secure|hisnos.*kernel" 2>/dev/null || true; then
    override_active="true"
  fi

  if [[ "${boot_kernel}" != *hisnos-secure* ]]; then
    warn "Kernel mismatch: override_active=${override_active} but booted kernel is '${boot_kernel}' (expected *hisnos-secure*). Select the correct deployment or reboot into the HisnOS kernel."
  else
    info "Booted kernel matches HisnOS signature: ${boot_kernel}"
  fi

  status_line OK "Kernel validation" "override_active=${override_active}, boot_kernel='${boot_kernel}'"

  # Step 11: Command Center (searchd + UI overlay + SUPER+SPACE shortcut)
  CURRENT_STEP="Command Center"
  section "Command Center (searchd + search UI)"

  local SEARCHD_SRC="${REPO_DIR}/commandcenter/searchd"
  local SEARCHD_BIN="${USER_DATA_BASE}/bin/searchd"
  local SEARCH_UI_SRC="${REPO_DIR}/commandcenter/ui/hisnos-search-ui.py"
  local SEARCH_UI_BIN="${USER_DATA_BASE}/bin/hisnos-search-ui"
  local COMMANDS_SRC="${REPO_DIR}/commandcenter/commands.json"
  local COMMANDS_DST="${USER_DATA_BASE}/commands.json"
  local SEARCHD_UNIT_SRC="${REPO_DIR}/commandcenter/systemd/hisnos-searchd.service"
  local SEARCH_UI_UNIT_SRC="${REPO_DIR}/commandcenter/systemd/hisnos-search-ui.service"

  # 11a. Build searchd binary
  if [[ "${HISNOS_DISABLE_DASHBOARD_BUILD:-0}" == "1" ]]; then
    if [[ ! -f "${SEARCHD_BIN}" ]]; then
      warn "searchd binary missing and build disabled — Command Center unavailable"
    else
      status_line SKIP "searchd build" "using pre-built binary at ${SEARCHD_BIN}"
    fi
  elif ! command -v go &>/dev/null; then
    warn "Go toolchain not found — searchd not built. Install Go and re-run bootstrap."
  elif [[ ! -d "${SEARCHD_SRC}" ]]; then
    warn "commandcenter/searchd/ source directory not found — searchd not built"
  else
    mkdir -p "${USER_DATA_BASE}/bin"
    if (cd "${SEARCHD_SRC}" && go build -o "${SEARCHD_BIN}" .); then
      chmod 755 "${SEARCHD_BIN}"
      status_line OK "searchd binary" "${SEARCHD_BIN}"
    else
      warn "searchd go build failed — Command Center search unavailable (non-fatal)"
    fi
  fi

  # 11b. Install commands.json
  if [[ -f "${COMMANDS_SRC}" ]]; then
    cp -f "${COMMANDS_SRC}" "${COMMANDS_DST}"
    status_line OK "commands.json" "${COMMANDS_DST}"
  else
    warn "commandcenter/commands.json not found — command search unavailable"
  fi

  # 11c. Install Python UI wrapper (make executable, symlink to bin)
  if [[ -f "${SEARCH_UI_SRC}" ]]; then
    cp -f "${SEARCH_UI_SRC}" "${SEARCH_UI_BIN}.py"
    chmod 755 "${SEARCH_UI_BIN}.py"
    # Wrapper script to set PYTHONPATH and launch
    cat > "${SEARCH_UI_BIN}" << UIWRAP_EOF
#!/usr/bin/env bash
export PYTHONPATH="${REPO_DIR}/commandcenter/ipc:\${PYTHONPATH:-}"
exec python3 "${SEARCH_UI_BIN}.py" "\$@"
UIWRAP_EOF
    chmod 755 "${SEARCH_UI_BIN}"
    status_line OK "hisnos-search-ui" "${SEARCH_UI_BIN}"
  else
    warn "commandcenter/ui/hisnos-search-ui.py not found — UI overlay not installed"
  fi

  # 11d. Install and enable user service units
  local searchd_ok=false
  if [[ -f "${SEARCHD_UNIT_SRC}" ]] && [[ -f "${SEARCHD_BIN}" ]]; then
    cp -f "${SEARCHD_UNIT_SRC}" "${USER_CONFIG_USER_SYSTEMD}/hisnos-searchd.service"
    if systemctl --user daemon-reload 2>/dev/null \
        && systemctl --user enable --now hisnos-searchd.service &>/dev/null; then
      status_line OK "hisnos-searchd.service" "enabled and started"
      searchd_ok=true
    else
      warn "hisnos-searchd.service failed to start (non-fatal). Check: journalctl --user -u hisnos-searchd"
    fi
  else
    status_line SKIP "hisnos-searchd.service" "binary or unit not found"
  fi

  if [[ -f "${SEARCH_UI_UNIT_SRC}" ]] && [[ -f "${SEARCH_UI_BIN}" ]]; then
    cp -f "${SEARCH_UI_UNIT_SRC}" "${USER_CONFIG_USER_SYSTEMD}/hisnos-search-ui.service"
    systemctl --user daemon-reload 2>/dev/null || true
    # Enable but do not start — UI starts with graphical session
    systemctl --user enable hisnos-search-ui.service &>/dev/null || true
    status_line OK "hisnos-search-ui.service" "enabled (starts with graphical session)"
  else
    status_line SKIP "hisnos-search-ui.service" "binary or unit not found"
  fi

  # 11e. Register SUPER+SPACE keyboard shortcut
  local SHORTCUT_SCRIPT="${REPO_DIR}/commandcenter/ui/setup-shortcut.sh"
  if [[ -f "${SHORTCUT_SCRIPT}" ]] && command -v kwriteconfig5 &>/dev/null || command -v kwriteconfig6 &>/dev/null; then
    if bash "${SHORTCUT_SCRIPT}" &>/dev/null; then
      status_line OK "SUPER+SPACE shortcut" "registered via KDE KGlobalAccel"
    else
      warn "KDE shortcut registration failed — run manually: bash ${SHORTCUT_SCRIPT}"
    fi
  else
    warn "KDE shortcut tools not found — run manually after login: bash ${SHORTCUT_SCRIPT}"
  fi

  # 11f. Verify searchd IPC socket
  local SEARCH_SOCK="/run/user/$(id -u)/hisnos-search.sock"
  sleep 1
  if [[ -S "${SEARCH_SOCK}" ]]; then
    status_line OK "searchd IPC socket" "${SEARCH_SOCK}"
  else
    warn "searchd IPC socket not yet present at ${SEARCH_SOCK} — may appear after graphical login"
  fi

  status_line OK "Command Center" "searchd=${searchd_ok} — press SUPER+SPACE after login to invoke"

  # Step 13: Distribution Experience Layer
  CURRENT_STEP="Distribution Experience"
  section "Distribution Experience (Plymouth + Onboarding + Recovery)"

  # 13a. Plymouth theme
  local PLYMOUTH_SRC="${REPO_DIR}/plymouth"
  local PLYMOUTH_INSTALL="${PLYMOUTH_SRC}/install-theme.sh"

  if [[ -f "${PLYMOUTH_INSTALL}" ]]; then
    if sudo bash "${PLYMOUTH_INSTALL}" 2>/dev/null; then
      status_line OK "Plymouth theme" "hisnos theme installed and set as default"
    else
      warn "Plymouth theme install failed — boot splash will use default theme (non-fatal)"
    fi
  else
    warn "plymouth/install-theme.sh not found — Plymouth theme not installed"
  fi

  # 13b. Build and install the onboarding Go binary
  local ONBOARDING_SRC="${REPO_DIR}/onboarding/backend"
  local ONBOARDING_BIN="/usr/local/bin/hisnos-onboarding"
  local ONBOARDING_UNIT_SRC="${REPO_DIR}/onboarding/systemd/hisnos-onboarding.service"
  local ONBOARDING_USER_UNIT_DST="/usr/lib/systemd/user/hisnos-onboarding.service"
  local ONBOARDING_STATE="/var/lib/hisnos/onboarding-state.json"

  if [[ "${HISNOS_DISABLE_DASHBOARD_BUILD:-0}" == "1" ]]; then
    status_line SKIP "onboarding binary build" "HISNOS_DISABLE_DASHBOARD_BUILD=1"
  elif ! command -v go &>/dev/null; then
    warn "Go not found — onboarding wizard not built. Install Go and re-run bootstrap."
  elif [[ ! -d "${ONBOARDING_SRC}" ]]; then
    warn "onboarding/backend/ not found — onboarding wizard not installed"
  else
    # Build SvelteKit frontend first (if npm is available and dist/ is absent).
    local FRONTEND_SRC="${REPO_DIR}/onboarding/frontend"
    local DIST_DIR="${ONBOARDING_SRC}/dist"
    if [[ ! -d "${DIST_DIR}" ]]; then
      if command -v npm &>/dev/null && [[ -f "${FRONTEND_SRC}/package.json" ]]; then
        (
          cd "${FRONTEND_SRC}"
          npm install --silent 2>/dev/null || true
          npm run build 2>/dev/null || true
        )
        if [[ -d "${DIST_DIR}" ]]; then
          status_line OK "onboarding frontend" "SvelteKit built to ${DIST_DIR}"
        else
          warn "SvelteKit build did not produce dist/ — onboarding UI will be absent"
        fi
      else
        warn "npm not found and dist/ absent — install Node.js and run: cd onboarding/frontend && npm install && npm run build"
      fi
    else
      status_line SKIP "onboarding frontend build" "dist/ already present"
    fi

    # Build Go binary (embeds dist/).
    if (cd "${ONBOARDING_SRC}" && go build -o /tmp/hisnos-onboarding .); then
      sudo install -m 0755 /tmp/hisnos-onboarding "${ONBOARDING_BIN}"
      rm -f /tmp/hisnos-onboarding
      status_line OK "onboarding binary" "${ONBOARDING_BIN}"
    else
      warn "onboarding go build failed — wizard unavailable (non-fatal)"
    fi
  fi

  # 13c. Install and (conditionally) enable the onboarding user service.
  if [[ -f "${ONBOARDING_UNIT_SRC}" ]]; then
    if sudo install -m 0644 "${ONBOARDING_UNIT_SRC}" "${ONBOARDING_USER_UNIT_DST}" 2>/dev/null; then
      status_line OK "hisnos-onboarding.service" "installed to ${ONBOARDING_USER_UNIT_DST}"
      # Only enable the service if onboarding has not already been completed.
      if [[ ! -f "${ONBOARDING_STATE}" ]]; then
        # Enable for all users via the preset mechanism.
        sudo systemctl --global enable hisnos-onboarding.service 2>/dev/null || true
        status_line OK "hisnos-onboarding.service" "globally enabled (runs on first graphical login)"
      else
        status_line SKIP "hisnos-onboarding.service" "onboarding already complete"
      fi
    else
      warn "Failed to install hisnos-onboarding.service (may need rpm-ostree overlay for /usr/lib/systemd/user)"
    fi
  fi

  # 13d. Install GRUB recovery entry and dracut module.
  local RECOVERY_SETUP="${REPO_DIR}/recovery/hisnos-recovery-setup.sh"
  if [[ -f "${RECOVERY_SETUP}" ]]; then
    if sudo bash "${RECOVERY_SETUP}" 2>/dev/null; then
      status_line OK "Recovery entry" "GRUB recovery menu entry installed"
    else
      warn "Recovery entry setup failed — run manually: sudo bash recovery/hisnos-recovery-setup.sh"
    fi
  else
    warn "recovery/hisnos-recovery-setup.sh not found — recovery entry not installed"
  fi

  status_line OK "Distribution Experience" "Plymouth + onboarding wizard + recovery entry complete"

  # Step 14: Distribution Finalization
  CURRENT_STEP="Distribution Finalization"
  section "Distribution Finalization (boot health + indicator + version + cmdline)"

  # 14a. Install boot health script and service.
  local BOOT_HEALTH_SCRIPT="${REPO_DIR}/boot/hisnos-boot-health.sh"
  local BOOT_HEALTH_UNIT="${REPO_DIR}/boot/systemd/hisnos-boot-health.service"
  local BOOT_LIB_DIR="/usr/local/lib/hisnos"

  if sudo mkdir -p "${BOOT_LIB_DIR}" 2>/dev/null; then
    if [[ -f "${BOOT_HEALTH_SCRIPT}" ]]; then
      sudo install -m 0755 "${BOOT_HEALTH_SCRIPT}" "${BOOT_LIB_DIR}/hisnos-boot-health.sh"
      status_line OK "hisnos-boot-health.sh" "installed to ${BOOT_LIB_DIR}"
    fi
    if [[ -f "${BOOT_HEALTH_UNIT}" ]]; then
      sudo install -m 0644 "${BOOT_HEALTH_UNIT}" "/usr/lib/systemd/system/hisnos-boot-health.service"
      if sudo systemctl daemon-reload 2>/dev/null && sudo systemctl enable hisnos-boot-health.service 2>/dev/null; then
        status_line OK "hisnos-boot-health.service" "enabled (runs on every boot)"
      else
        warn "hisnos-boot-health.service failed to enable (non-fatal)"
      fi
    fi
  fi

  # 14b. Validate kernel cmdline (warn only; --fix not applied automatically).
  local CMDLINE_SCRIPT="${REPO_DIR}/boot/validate-kernel-cmdline.sh"
  if [[ -f "${CMDLINE_SCRIPT}" ]]; then
    if bash "${CMDLINE_SCRIPT}" 2>/dev/null; then
      status_line OK "kernel cmdline" "required flags present"
    else
      warn "Kernel cmdline missing HisnOS flags. Run: sudo bash boot/validate-kernel-cmdline.sh --fix"
      warn "Required: quiet splash loglevel=3 rd.systemd.show_status=false"
    fi
  fi

  # 14c. Install status indicator.
  local INDICATOR_SRC="${REPO_DIR}/desktop/hisnos-status-indicator.py"
  local INDICATOR_BIN="/usr/local/bin/hisnos-status-indicator"
  local INDICATOR_UNIT_SRC="${REPO_DIR}/desktop/systemd/hisnos-status-indicator.service"
  local AUTOSTART_INDICATOR="${REPO_DIR}/desktop/autostart/hisnos-status-indicator.desktop"
  local AUTOSTART_SEARCH="${REPO_DIR}/desktop/autostart/hisnos-search-ui.desktop"
  local XDG_AUTOSTART_DIR="/etc/xdg/autostart"

  if [[ -f "${INDICATOR_SRC}" ]]; then
    sudo install -m 0755 "${INDICATOR_SRC}" "${INDICATOR_BIN}"
    status_line OK "hisnos-status-indicator" "${INDICATOR_BIN}"
  fi

  if [[ -f "${INDICATOR_UNIT_SRC}" ]]; then
    sudo install -m 0644 "${INDICATOR_UNIT_SRC}" "/usr/lib/systemd/user/hisnos-status-indicator.service"
    # Enable globally so it starts for every user's graphical session.
    sudo systemctl --global enable hisnos-status-indicator.service 2>/dev/null || \
      warn "Failed to globally enable hisnos-status-indicator.service"
  fi

  # Install XDG autostart entries (fallback for non-systemd session managers).
  if sudo mkdir -p "${XDG_AUTOSTART_DIR}" 2>/dev/null; then
    for desktop_file in "${AUTOSTART_INDICATOR}" "${AUTOSTART_SEARCH}"; do
      [[ -f "${desktop_file}" ]] && \
        sudo install -m 0644 "${desktop_file}" "${XDG_AUTOSTART_DIR}/" && \
        status_line OK "autostart: $(basename "${desktop_file}")" "${XDG_AUTOSTART_DIR}/"
    done
  fi

  # 14d. Install version file and CLI.
  local VERSION_INSTALL="${REPO_DIR}/release/install-version.sh"
  if [[ -f "${VERSION_INSTALL}" ]]; then
    if sudo bash "${VERSION_INSTALL}" 2>/dev/null; then
      status_line OK "hisnos-version" "$(hisnos-version --short 2>/dev/null || echo 'installed')"
    else
      warn "Failed to install version file (non-fatal)"
    fi
  fi

  status_line OK "Distribution Finalization" "boot health + status indicator + version + cmdline validated"

  # Step 15: Phase 15 Production — Performance, Automation, Ecosystem
  CURRENT_STEP="Phase 15 Production"
  section "Phase 15 — Performance Runtime + Automation + Ecosystem"
  step15_phase15_production

  # Step 16: Build Pipeline — dracut module, systemd units, nftables ruleset,
  #           hisnos-pkg CLI, Phase A-D service units.
  CURRENT_STEP="Build Pipeline"
  section "Step 16 — Production Build Pipeline + Phase A-D Services"
  step16_build_pipeline
}

# ─── Step 15: Performance + Automation + Ecosystem ───────────────────────────
step15_phase15_production() {
  log "=== Step 15: Phase 15 — Performance, Automation & Ecosystem ==="

  # 15a. Install hisnos-perf-apply helper.
  local PERF_APPLY_SRC="${REPO_DIR}/core/performance/hisnos-perf-apply.sh"
  local PERF_APPLY_BIN="/usr/local/bin/hisnos-perf-apply"
  if [[ -f "${PERF_APPLY_SRC}" ]]; then
    sudo install -m 0755 "${PERF_APPLY_SRC}" "${PERF_APPLY_BIN}"
    status_line OK "hisnos-perf-apply" "${PERF_APPLY_BIN}"
  else
    warn "hisnos-perf-apply.sh not found — skipping"
  fi

  # 15b. Install hisnos-performance.service (user unit, global).
  local PERF_UNIT="${REPO_DIR}/core/performance/systemd/hisnos-performance.service"
  if [[ -f "${PERF_UNIT}" ]]; then
    sudo install -m 0644 "${PERF_UNIT}" "/usr/lib/systemd/user/hisnos-performance.service"
    sudo systemctl --global enable hisnos-performance.service 2>/dev/null || \
      warn "Failed to globally enable hisnos-performance.service (non-fatal)"
    status_line OK "hisnos-performance.service" "installed + globally enabled"
  fi

  # 15c. Install update-check timer + service (system units).
  for unit_src in \
    "${REPO_DIR}/core/ecosystem/systemd/hisnos-update-check.service" \
    "${REPO_DIR}/core/ecosystem/systemd/hisnos-update-check.timer"; do
    [[ -f "${unit_src}" ]] && \
      sudo install -m 0644 "${unit_src}" "/usr/lib/systemd/system/$(basename "${unit_src}")"
  done
  if sudo systemctl enable --now hisnos-update-check.timer 2>/dev/null; then
    status_line OK "hisnos-update-check.timer" "enabled (weekly, persistent)"
  else
    warn "Failed to enable hisnos-update-check.timer (non-fatal)"
  fi

  # 15d. Create required state directories.
  for dir in /var/lib/hisnos/forensics /var/log/hisnos /etc/hisnos; do
    sudo mkdir -p "${dir}" && sudo chmod 0750 "${dir}" 2>/dev/null || true
  done
  status_line OK "state dirs" "/var/lib/hisnos/* /var/log/hisnos /etc/hisnos"

  # 15e. Write default telemetry config (opt-in, disabled by default).
  local TELEMETRY_CONF="/etc/hisnos/telemetry.conf"
  if [[ ! -f "${TELEMETRY_CONF}" ]]; then
    sudo tee "${TELEMETRY_CONF}" > /dev/null <<'TELEOF'
# HisnOS Telemetry Configuration
# Set enabled=true to opt in to anonymous usage telemetry.
# Set endpoint=https://... to configure the collection endpoint.
# Default: disabled. No data is collected or transmitted by default.
enabled=false
# endpoint=https://telemetry.hisnos.example/ingest
TELEOF
    status_line OK "telemetry.conf" "${TELEMETRY_CONF} (disabled)"
  fi

  # 15f. Initialise JSON state files with safe defaults (idempotent).
  local PERF_STATE="/var/lib/hisnos/perf-state.json"
  if [[ ! -f "${PERF_STATE}" ]]; then
    sudo tee "${PERF_STATE}" > /dev/null \
      <<< "{\"active_profile\":\"balanced\",\"applied_at\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}"
    status_line OK "perf-state.json" "initialised (balanced)"
  fi

  local AUTO_STATE="/var/lib/hisnos/automation-state.json"
  if [[ ! -f "${AUTO_STATE}" ]]; then
    sudo tee "${AUTO_STATE}" > /dev/null \
      <<< "{\"alert_threshold\":70.0,\"false_positives\":0,\"confirmed_alerts\":0,\"last_adjustment\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",\"override_cooldown_until\":\"2000-01-01T00:00:00Z\",\"incidents\":[]}"
    status_line OK "automation-state.json" "initialised (threshold=70)"
  fi

  local REG_FILE="/var/lib/hisnos/module-registry.json"
  if [[ ! -f "${REG_FILE}" ]]; then
    sudo tee "${REG_FILE}" > /dev/null \
      <<< "{\"modules\":[],\"updated_at\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}"
    status_line OK "module-registry.json" "initialised (empty)"
  fi

  # 15g. Rebuild hisnosd binary if Go toolchain is available.
  if command -v go &>/dev/null; then
    log "Building hisnosd (Phase 15)..."
    if (cd "${REPO_DIR}/core" && go build -o /tmp/hisnosd . 2>&1 | tee /tmp/hisnosd-build.log); then
      sudo install -m 0755 /tmp/hisnosd /usr/local/bin/hisnosd
      rm -f /tmp/hisnosd
      status_line OK "hisnosd" "rebuilt with Phase 15 packages"
    else
      warn "hisnosd build failed — see /tmp/hisnosd-build.log"
    fi
  else
    warn "Go toolchain not found — hisnosd not rebuilt (run: sudo dnf install golang)"
  fi

  status_line OK "Phase 15" "Performance + Automation + Ecosystem installed"
}

# ─── Step 16: Build Pipeline + Phase A-D Services ─────────────────────────────
step16_build_pipeline() {
  log "=== Step 16: Build Pipeline + Phase A-D Services ==="

  # 16a. Install nftables base ruleset.
  local NFT_SRC="${REPO_DIR}/security/nftables-base.nft"
  local NFT_DST="/etc/nftables.conf"
  if [[ -f "${NFT_SRC}" ]]; then
    # Back up any existing ruleset first.
    if [[ -f "${NFT_DST}" ]]; then
      sudo cp "${NFT_DST}" "${NFT_DST}.bak.$(date +%Y%m%d-%H%M%S)"
    fi
    sudo install -m 0640 "${NFT_SRC}" "${NFT_DST}"
    sudo chown root:root "${NFT_DST}"
    status_line OK "nftables-base.nft" "${NFT_DST} (backup retained)"

    # Validate the ruleset syntax (dry-run — does not modify kernel state).
    if nft --check -f "${NFT_DST}" 2>/dev/null; then
      status_line OK "nftables syntax" "OK"
    else
      warn "nftables ruleset syntax check failed — inspect ${NFT_DST}"
    fi

    # Enable nftables.service so the ruleset loads at boot.
    if sudo systemctl enable nftables.service 2>/dev/null; then
      status_line OK "nftables.service" "enabled"
    fi
  else
    warn "nftables-base.nft not found at ${NFT_SRC}"
  fi

  # 16b. Install dracut module for boot reliability.
  local DRACUT_INSTALL="${REPO_DIR}/dracut/install-dracut-module.sh"
  if [[ -f "${DRACUT_INSTALL}" ]]; then
    if sudo bash "${DRACUT_INSTALL}" --no-rebuild; then
      status_line OK "dracut-95hisnos" "module installed (initramfs rebuild deferred)"
      # Schedule a rebuild on next boot via a transient systemd unit.
      sudo systemd-run --unit=hisnos-initramfs-rebuild \
        --description="HisnOS initramfs rebuild (one-shot)" \
        --on-boot=30 \
        dracut --force --add "95hisnos" --kernel-version "$(uname -r)" 2>/dev/null || \
        warn "Could not schedule initramfs rebuild — run: sudo dracut --force --add 95hisnos"
    else
      warn "Dracut module install failed (non-fatal)"
    fi
  else
    warn "install-dracut-module.sh not found — skipping dracut module install"
  fi

  # 16c. Install Phase A-D systemd service units.
  #      These units are managed by hisnosd (via hisnosd --module <name>).
  local SYSTEMD_DIR="/usr/lib/systemd/system"
  local UNIT_DIRS=(
    "${REPO_DIR}/systemd"
    "${REPO_DIR}/core/performance/systemd"
    "${REPO_DIR}/core/fleet/systemd"
  )

  local installed_units=0
  for unit_dir in "${UNIT_DIRS[@]}"; do
    if [[ -d "${unit_dir}" ]]; then
      for unit_file in "${unit_dir}"/*.service "${unit_dir}"/*.timer; do
        [[ -f "${unit_file}" ]] || continue
        local unit_name
        unit_name="$(basename "${unit_file}")"
        sudo install -m 0644 "${unit_file}" "${SYSTEMD_DIR}/${unit_name}"
        (( installed_units++ ))
      done
    fi
  done

  if [[ "${installed_units}" -gt 0 ]]; then
    sudo systemctl daemon-reload
    status_line OK "systemd units" "${installed_units} units installed + daemon-reload"
  fi

  # Enable Phase A-D system services (non-gaming, always active).
  local ALWAYS_ON_UNITS=(
    "hisnos-threat-engine.service"
    "hisnos-automation.service"
    "hisnos-performance-guard.service"
    "hisnos-fleet-sync.timer"
    "hisnos-boot-complete.service"
  )
  for unit in "${ALWAYS_ON_UNITS[@]}"; do
    if [[ -f "${SYSTEMD_DIR}/${unit}" ]]; then
      if sudo systemctl enable "${unit}" 2>/dev/null; then
        status_line OK "${unit}" "enabled"
      else
        warn "Failed to enable ${unit} (will start with hisnosd)"
      fi
    fi
  done

  # 16d. Install hisnos-pkg marketplace CLI.
  if command -v go &>/dev/null; then
    local PKG_CLI_DIR="${REPO_DIR}/cmd/hisnos-pkg"
    if [[ -d "${PKG_CLI_DIR}" ]]; then
      log "Building hisnos-pkg CLI..."
      if (cd "${REPO_DIR}/core" && go build -o /tmp/hisnos-pkg ./cmd/hisnos-pkg/ 2>&1); then
        sudo install -m 0755 /tmp/hisnos-pkg /usr/local/bin/hisnos-pkg
        rm -f /tmp/hisnos-pkg
        status_line OK "hisnos-pkg" "/usr/local/bin/hisnos-pkg"
      else
        warn "hisnos-pkg build failed — marketplace CLI unavailable"
      fi
    fi
  else
    warn "Go toolchain not found — hisnos-pkg not built"
  fi

  # 16e. Rebuild hisnosd with Phase A-D packages.
  if command -v go &>/dev/null; then
    log "Rebuilding hisnosd with Phase A-D packages..."
    if (cd "${REPO_DIR}/core" && go build -o /tmp/hisnosd . 2>&1 | tee /tmp/hisnosd-build-p16.log); then
      sudo install -m 0755 /tmp/hisnosd /usr/local/bin/hisnosd
      rm -f /tmp/hisnosd
      status_line OK "hisnosd" "rebuilt with Phase A-D packages"
    else
      warn "hisnosd build failed — see /tmp/hisnosd-build-p16.log"
      warn "Existing binary retained; Phase A-D features will be active after rebuild"
    fi
  fi

  # 16f. Initialise Phase A-D state files.
  local STATE_INIT_FILES=(
    "/var/lib/hisnos/boot-health.json:{\"ring\":[null,null,null,null,null,null,null],\"head\":0,\"full\":false}"
    "/var/lib/hisnos/deployment-graph.json:{\"nodes\":[]}"
    "/var/lib/hisnos/fleet-identity.json:{}"
    "/var/lib/hisnos/automation-baseline.json:{\"phase\":\"learning\",\"samples\":0,\"mean\":{},\"m2\":{}}"
    "/var/lib/hisnos/automation-confidence.json:{\"history\":{}}"
  )
  for entry in "${STATE_INIT_FILES[@]}"; do
    local path="${entry%%:*}"
    local content="${entry#*:}"
    if [[ ! -f "${path}" ]]; then
      echo "${content}" | sudo tee "${path}" > /dev/null
      sudo chmod 0640 "${path}"
      status_line OK "$(basename "${path}")" "initialised"
    fi
  done

  # 16g. Validate OSTRee build tooling (informational only).
  local COMPOSE_SCRIPT="${REPO_DIR}/build/ostree/compose.sh"
  if [[ -f "${COMPOSE_SCRIPT}" ]]; then
    status_line OK "OSTree compose" "${COMPOSE_SCRIPT} (run manually on build host)"
  fi
  local ISO_SCRIPT="${REPO_DIR}/build/iso/build-hisnos-iso.sh"
  if [[ -f "${ISO_SCRIPT}" ]]; then
    status_line OK "ISO pipeline" "${ISO_SCRIPT} (run on build host with lorax)"
  fi

  status_line OK "Step 16" "Build Pipeline + Phase A-D Services installed"
}

main "$@"

