#!/usr/bin/env bash
# release/install-version.sh
#
# Installs /etc/hisnos-release and /usr/local/bin/hisnos-version.
# Called during bootstrap (step 13) and from the Calamares shellprocess module.
#
# Usage: sudo bash install-version.sh [--version X.Y] [--build N] [--channel stable|beta]

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

VERSION="1.0"
BUILD="1"
CHANNEL="stable"
BASE_OS="Fedora Kinoite 40"
CODENAME="Fortress"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --build)   BUILD="$2";   shift 2 ;;
    --channel) CHANNEL="$2"; shift 2 ;;
    *) echo "Unknown option: $1" >&2; exit 1 ;;
  esac
done

[[ "$(id -u)" -eq 0 ]] || { echo "ERROR: must run as root" >&2; exit 1; }

BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
# Git commit if available.
BUILD_COMMIT=$(git -C "${SCRIPT_DIR}" rev-parse --short HEAD 2>/dev/null || echo "unknown")

# ── Write /etc/hisnos-release ─────────────────────────────────────────────────
cat > /etc/hisnos-release << EOF
VERSION=${VERSION}
BUILD=${BUILD}
CHANNEL=${CHANNEL}
BASE_OS=${BASE_OS}
CODENAME=${CODENAME}
BUILD_DATE=${BUILD_DATE}
BUILD_COMMIT=${BUILD_COMMIT}
EOF
chmod 0644 /etc/hisnos-release
echo "[install-version] /etc/hisnos-release written"

# ── Write hisnos-version CLI ──────────────────────────────────────────────────
cat > /usr/local/bin/hisnos-version << 'VERSIONEOF'
#!/usr/bin/env bash
# hisnos-version — Display HisnOS version information.
#
# Usage: hisnos-version [--json] [--short]

set -euo pipefail

RELEASE_FILE="/etc/hisnos-release"
SHORT=false
JSON=false

for arg in "$@"; do
  case "${arg}" in
    --short) SHORT=true ;;
    --json)  JSON=true  ;;
    --help|-h)
      echo "Usage: hisnos-version [--short] [--json]"
      exit 0
      ;;
  esac
done

if [[ ! -f "${RELEASE_FILE}" ]]; then
  echo "ERROR: ${RELEASE_FILE} not found. Is HisnOS installed?" >&2
  exit 1
fi

# Source the release file as variables.
# shellcheck disable=SC1090
source "${RELEASE_FILE}"

# Gather additional runtime info.
KERNEL=$(uname -r)
OSTREE_BOOTED=""
if command -v rpm-ostree &>/dev/null; then
  OSTREE_BOOTED=$(rpm-ostree status --json 2>/dev/null \
    | python3 -c "import sys,json; d=json.load(sys.stdin); [print(d['deployments'][i]['checksum'][:12]) for i in range(len(d['deployments'])) if d['deployments'][i].get('booted')]" 2>/dev/null \
    || echo "unknown")
fi

if [[ "${SHORT}" == "true" ]]; then
  echo "${VERSION:-unknown}"
  exit 0
fi

if [[ "${JSON}" == "true" ]]; then
  python3 - << PYEOF
import json
data = {
  "version":      "${VERSION:-unknown}",
  "build":        "${BUILD:-unknown}",
  "channel":      "${CHANNEL:-unknown}",
  "codename":     "${CODENAME:-unknown}",
  "base_os":      "${BASE_OS:-unknown}",
  "build_date":   "${BUILD_DATE:-unknown}",
  "build_commit": "${BUILD_COMMIT:-unknown}",
  "kernel":       "${KERNEL}",
  "ostree_commit":"${OSTREE_BOOTED}",
}
print(json.dumps(data, indent=2))
PYEOF
  exit 0
fi

# Human-readable output.
cat << EOF
HisnOS ${VERSION:-unknown} (${CODENAME:-unknown})
  Channel   : ${CHANNEL:-unknown}
  Build     : ${BUILD:-unknown}
  Build date: ${BUILD_DATE:-unknown}
  Commit    : ${BUILD_COMMIT:-unknown}
  Base OS   : ${BASE_OS:-unknown}
  Kernel    : ${KERNEL}
  OSTree    : ${OSTREE_BOOTED:-n/a}
EOF
VERSIONEOF

chmod 0755 /usr/local/bin/hisnos-version
echo "[install-version] /usr/local/bin/hisnos-version installed"

echo "[install-version] Done. Test: hisnos-version"
