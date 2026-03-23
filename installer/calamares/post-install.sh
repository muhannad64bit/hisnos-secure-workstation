#!/usr/bin/env bash
# installer/calamares/post-install.sh
#
# Wrapper executed by the Calamares shellprocess module (see
# shellprocess-hisnos-bootstrap.conf).  Runs inside the target chroot.
#
# This script exists to give the Calamares module a single, stable entry
# point regardless of the internal layout of the HisnOS source tree.
#
# Usage (called by Calamares — not meant for direct invocation):
#   bash post-install.sh

set -euo pipefail

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BOOTSTRAP="${SRC_DIR}/bootstrap/bootstrap-installer.sh"

echo "[hisnos-post-install] Source dir: ${SRC_DIR}"

if [[ ! -f "${BOOTSTRAP}" ]]; then
  echo "ERROR: bootstrap-installer.sh not found at ${BOOTSTRAP}" >&2
  exit 1
fi

bash "${BOOTSTRAP}"

echo "[hisnos-post-install] Bootstrap complete."
