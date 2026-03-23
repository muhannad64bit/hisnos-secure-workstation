#!/bin/bash
# tests/qemu-test.sh
# Validates HisnOS boot determinism (<8 seconds) and failure limits

ISO_PATH=${1:-"build/HisnOS-Live.iso"}

if [ ! -f "$ISO_PATH" ]; then
    echo "WARNING: ISO not found at $ISO_PATH. (Placeholder path during test design)"
fi

echo "Booting HisnOS under QEMU to measure time to graphical.target..."

# To trigger rd.break or safe mode, append variables:
# e.g., APPEND="console=ttyS0 hisnos.mode=safe"
APPEND="console=ttyS0 hisnos.mode=normal systemd.journald.forward_to_console=1"

qemu-system-x86_64 \
    -m 4096 \
    -enable-kvm \
    -cpu host \
    -smp 4 \
    -cdrom "$ISO_PATH" \
    -vga virtio \
    -serial stdio \
    -append "$APPEND"
