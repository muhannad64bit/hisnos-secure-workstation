# =============================================================================
# kickstart/hisnos-install.ks — HisnOS Secure Workstation Kickstart
#
# Full automated installation of HisnOS using Anaconda.
# Compatible with Fedora 43 / rpm-ostree based installations.
#
# Usage modes:
#   1. ISO-embedded:  inst.ks=cdrom:/ks/hisnos-install.ks
#   2. Network:       inst.ks=https://install.hisnos.dev/ks/hisnos-install.ks
#   3. USB:           inst.ks=hd:sdb1:/hisnos-install.ks
#
# Post-install:
#   - Runs /usr/local/lib/hisnos/bootstrap/bootstrap-installer.sh
#   - Installs hisnosd + dashboard
#   - Configures vault, egress, audit
#   - First-boot wizard: hisnos-onboarding.service
#
# Disk layout (adaptable via %include):
#   /boot/efi  → vfat  512 MiB
#   /boot      → ext4  1 GiB
#   /          → btrfs Remaining (with subvols: @, @home, @var, @log)
# =============================================================================

# ─── Installation method ─────────────────────────────────────────────────────
# Use OSTree-based installation (Silverblue/Kinoite model)
ostreesetup --nogpg \
    --osname=hisnos \
    --remote=hisnos \
    --url=file:///run/install/repo/ostree/repo \
    --ref=hisnos/stable/x86_64

# ─── Language and locale ─────────────────────────────────────────────────────
lang en_US.UTF-8
keyboard --vckeymap=us --xlayouts=us
timezone UTC --utc

# ─── Network ─────────────────────────────────────────────────────────────────
network --bootproto=dhcp --device=link --activate --onboot=yes
network --hostname=hisnos-workstation

# ─── Security ────────────────────────────────────────────────────────────────
selinux --enforcing
firewall --enabled --service=ssh

# Root account: disabled (operator uses sudo)
rootpw --lock

# ─── User account ────────────────────────────────────────────────────────────
# Default admin user — will be reconfigured by first-boot wizard
user --name=admin \
     --gecos="HisnOS Administrator" \
     --groups=wheel,video,audio,plugdev,input,kvm,libvirt \
     --shell=/bin/bash \
     --password=hisnos-change-me \
     --iscrypted=no

# ─── Boot loader ─────────────────────────────────────────────────────────────
bootloader \
    --location=mbr \
    --boot-drive=sda \
    --append="audit=1 systemd.unified_cgroup_hierarchy=1 transparent_hugepage=madvise quiet splash loglevel=3"

# ─── Partition layout ────────────────────────────────────────────────────────
# Wipe partition table on primary disk
clearpart --all --initlabel --drives=sda

# EFI System Partition
part /boot/efi \
    --fstype=efi \
    --size=512 \
    --fsoptions="umask=0077,shortname=winnt"

# Boot partition
part /boot \
    --fstype=ext4 \
    --size=1024 \
    --label=hisnos-boot

# Root (btrfs with subvolumes)
part pv.root \
    --fstype=lvmpv \
    --size=1 \
    --grow

volgroup vg_hisnos pv.root

# Root volume — btrfs for snapshots + subvols
logvol / \
    --fstype=btrfs \
    --name=lv_root \
    --vgname=vg_hisnos \
    --size=1 \
    --grow \
    --label=hisnos-root

# Separate /home for data preservation on re-install
logvol /home \
    --fstype=btrfs \
    --name=lv_home \
    --vgname=vg_hisnos \
    --size=20480

# ─── Packages ────────────────────────────────────────────────────────────────
# Minimal — base packages are in the OSTree commit.
# Only add packages that must be outside the commit:
%packages --ignoremissing
@^workstation-product-environment
calamares
%end

# ─── Pre-installation ────────────────────────────────────────────────────────
%pre --log=/tmp/ks-pre.log --erroronfail
#!/bin/bash
set -e

echo "[hisnos-ks-pre] Starting pre-install checks..."

# Verify we have at least 20 GiB of disk space
DISK_GB=$(lsblk -b -d -o SIZE /dev/sda 2>/dev/null | tail -1 | awk '{print int($1/1073741824)}')
if [[ -n "$DISK_GB" ]] && [[ "$DISK_GB" -lt 20 ]]; then
    echo "[hisnos-ks-pre] ERROR: Disk too small: ${DISK_GB}GiB (need 20GiB+)"
    exit 1
fi
echo "[hisnos-ks-pre] Disk: ${DISK_GB:-unknown}GiB ✔"

# Detect UEFI vs BIOS
if [[ -d /sys/firmware/efi ]]; then
    echo "[hisnos-ks-pre] Boot mode: UEFI"
else
    echo "[hisnos-ks-pre] Boot mode: BIOS/Legacy"
fi

echo "[hisnos-ks-pre] Pre-install checks passed"
%end

# ─── Post-installation ───────────────────────────────────────────────────────
%post --log=/tmp/ks-post.log --erroronfail
#!/bin/bash
set -euo pipefail

echo "[hisnos-ks-post] Starting post-installation..."

# ── Set HisnOS release identity ──
mkdir -p /etc/hisnos
cat > /etc/hisnos/release <<'EOF'
HISNOS_VERSION=1.0
HISNOS_VARIANT=secure-workstation
HISNOS_BASE=fedora-43
HISNOS_INSTALL_METHOD=kickstart
EOF

# ── Copy HisnOS runtime from ISO ──
RUNTIME_SRC="/run/install/repo/LiveOS/hisnos-src"
if [[ -d "${RUNTIME_SRC}" ]]; then
    echo "[hisnos-ks-post] Installing HisnOS runtime..."
    mkdir -p /usr/local/lib/hisnos
    rsync -a "${RUNTIME_SRC}/" /usr/local/lib/hisnos/ 2>/dev/null || true
    echo "[hisnos-ks-post] Runtime installed"
else
    echo "[hisnos-ks-post] WARN: HisnOS runtime source not found at ${RUNTIME_SRC}"
fi

# ── Copy kickstart files ──
KS_SRC="/run/install/repo/ks"
if [[ -d "${KS_SRC}" ]]; then
    mkdir -p /usr/local/lib/hisnos/kickstart
    rsync -a "${KS_SRC}/" /usr/local/lib/hisnos/kickstart/ 2>/dev/null || true
fi

# ── Security hardening: sysctl ──
cat > /etc/sysctl.d/90-hisnos-hardening.conf <<'EOF'
# HisnOS security defaults
kernel.kptr_restrict = 2
kernel.dmesg_restrict = 1
kernel.perf_event_paranoid = 3
kernel.randomize_va_space = 2
kernel.yama.ptrace_scope = 2
net.core.bpf_jit_harden = 2
net.ipv4.conf.all.rp_filter = 1
net.ipv4.conf.default.rp_filter = 1
net.ipv4.conf.all.send_redirects = 0
net.ipv4.conf.all.accept_redirects = 0
net.ipv6.conf.all.accept_redirects = 0
net.ipv4.tcp_syncookies = 1
fs.protected_hardlinks = 1
fs.protected_symlinks = 1
fs.suid_dumpable = 0
EOF

# ── SELinux enforcing ──
if [[ -d /etc/selinux ]]; then
    cat > /etc/selinux/config <<'EOF'
SELINUX=enforcing
SELINUXTYPE=targeted
EOF
fi

# ── Enable core services ──
for svc in \
    NetworkManager \
    auditd \
    nftables \
    sddm \
    fstrim.timer \
    systemd-resolved; do
    systemctl enable "$svc" 2>/dev/null || true
done

# ── Generate SSH host keys ──
ssh-keygen -A 2>/dev/null || true

# ── First-boot: run bootstrap installer ──
BOOTSTRAP="/usr/local/lib/hisnos/bootstrap/bootstrap-installer.sh"
if [[ -f "$BOOTSTRAP" ]]; then
    echo "[hisnos-ks-post] Scheduling bootstrap for first boot..."
    cat > /etc/systemd/system/hisnos-first-boot.service <<EOF2
[Unit]
Description=HisnOS First Boot Bootstrap
ConditionPathExists=/etc/hisnos/release
ConditionPathExists=!/etc/hisnos/bootstrap-complete
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/bin/bash ${BOOTSTRAP}
ExecStartPost=/bin/touch /etc/hisnos/bootstrap-complete
RemainAfterExit=yes
StandardOutput=journal+console
StandardError=journal+console
TimeoutStartSec=600

[Install]
WantedBy=multi-user.target
EOF2
    systemctl enable hisnos-first-boot.service 2>/dev/null || true
fi

# ── zram setup ──
if [[ -d /etc/systemd/zram-generator.conf.d ]]; then
    cat > /etc/systemd/zram-generator.conf.d/hisnos-zram.conf <<'EOF'
[zram0]
zram-size = min(ram, 8192)
compression-algorithm = zstd
EOF
elif command -v zram-generator &>/dev/null; then
    cat > /etc/systemd/zram-generator.conf <<'EOF'
[zram0]
zram-size = min(ram, 8192)
compression-algorithm = zstd
EOF
fi

# ── Kernel command line hardening ──
GRUB_CONF="/etc/default/grub"
if [[ -f "$GRUB_CONF" ]]; then
    # Append security parameters
    sed -i '/^GRUB_CMDLINE_LINUX=/s/"$/ audit=1 systemd.unified_cgroup_hierarchy=1 transparent_hugepage=madvise"/' \
        "$GRUB_CONF" 2>/dev/null || true
fi

echo "[hisnos-ks-post] Post-installation complete"
%end

# ─── Post (no-chroot) ────────────────────────────────────────────────────────
%post --nochroot --log=/tmp/ks-post-nochroot.log
#!/bin/bash

# Copy log files to installed system for diagnostics
mkdir -p /mnt/sysimage/var/log/hisnos-install 2>/dev/null || true
cp /tmp/ks-pre.log  /mnt/sysimage/var/log/hisnos-install/ 2>/dev/null || true
cp /tmp/ks-post.log /mnt/sysimage/var/log/hisnos-install/ 2>/dev/null || true
echo "[ks-nochroot] Installation logs copied"
%end
