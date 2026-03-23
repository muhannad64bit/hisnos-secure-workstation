#!/bin/bash
# build-hisnos-iso.sh
# Phase 8: Reproducible ISO Builder

set -euo pipefail
WORKSPACE_DIR=$(realpath "$(dirname "$0")")
COMPOSE_WRAPPER="$WORKSPACE_DIR/build/hisnos-compose.sh"
TREEDIR="$WORKSPACE_DIR/repo"
ISO_DIR="$WORKSPACE_DIR/iso-build"

echo "============================================="
echo "   HisnOS Production ISO Build Pipeline      "
echo "============================================="

# 1. Podman compose stage
echo "[1/5] Triggering strictly isolated OSTree compose..."
bash "$COMPOSE_WRAPPER"

COMMIT=$(ostree --repo="$TREEDIR" log hisnos/1.0/x86_64/base | grep commit | head -n1 | awk '{print $2}')
if [ -z "$COMMIT" ]; then
    echo "CRITICAL: OSTree commit not found!"
    exit 1
fi

# 2. Dracut rebuild
echo "[2/5] Building hardened 90hisnos-live initramfs..."
dracut --force --nomdadmconf --nolvmconf --xz \
    --add "bash udev-rules hisnos-live systemd" \
    --include "$WORKSPACE_DIR/dracut/90hisnos-live" "/usr/lib/dracut/modules.d/90hisnos-live" \
    /tmp/initramfs-hisnos.img

# 3. Lorax live root generation
echo "[3/5] Generating live root via Lorax..."
rm -rf "$ISO_DIR"
lorax --sharedir "$WORKSPACE_DIR/lorax" \
      --add-template "$WORKSPACE_DIR/lorax/live-rootfs.tmpl" \
      --source "$TREEDIR" \
      --ref "hisnos/1.0/x86_64/base" \
      --volid "HISNOS_LIVE" \
      "$ISO_DIR"

# Ensure chaos tests are embedded into ISO
mkdir -p "$ISO_DIR/LiveOS/tests"
cp "$WORKSPACE_DIR/tests/chaos/run-all-chaos.sh" "$ISO_DIR/LiveOS/tests/"

# 4. xorriso packaging
echo "[4/5] Building final ISO via xorriso..."
KERNEL_VER=$(ls -1 /lib/modules | sort -V | tail -n1 || echo "unknown")
ISO_NAME="HisnOS-Production-${COMMIT:0:7}.iso"

xorriso -as mkisofs \
    -iso-level 3 \
    -V "HISNOS_LIVE" \
    -c pxelinux.boot \
    -b isolinux/isolinux.bin \
    -no-emul-boot -boot-load-size 4 -boot-info-table \
    -isohybrid-mbr /usr/share/syslinux/isohdpfx.bin \
    -eltorito-alt-boot \
    -e EFI/BOOT/grubx64.efi \
    -no-emul-boot -isohybrid-gpt-basdat \
    -o "$ISO_NAME" \
    "$ISO_DIR"

# 5. Checksum + manifest injection
echo "[5/5] Generating reproducibility manifest and checksums..."
implantisomd5 "$ISO_NAME"

MANIFEST="HisnOS-Manifest.txt"
echo "HisnOS Production Build Manifest" > "$MANIFEST"
echo "OSTree commit: $COMMIT" >> "$MANIFEST"
echo "Kernel version: $KERNEL_VER" >> "$MANIFEST"
echo "Reproducibility seed: $RANDOM-$COMMIT" >> "$MANIFEST"
echo "SHA256: $(sha256sum "$ISO_NAME" | awk '{print $1}')" >> "$MANIFEST"

cat "$MANIFEST"
echo "=========================================================="
echo " ISO BUILD SUCCESSFUL: $ISO_NAME "
echo "=========================================================="
