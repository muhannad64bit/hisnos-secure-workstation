# HisnOS Hardware Debugging Flow

When HisnOS encounters severe hardware conflicts (unstable GPU drivers, ACPI lockups, broken IRQ routing), the typical `graphical.target` boot may hang silently on standard distributions. HisnOS is designed to actively guard against this.

## The Triaging Pipeline

If the machine spins up but the UI never appears:
1. **Wait 15 Seconds**: The `hisnos-boot-validator.service` executes in the critical path. If `display-manager.service` fails to launch within the bounds, the system automatically pivots to `hisnos-safe.target`.
2. **Watchdog Intervention**: If the kernel itself encounters a soft lockup preventing systemd from acting, `99-hisnos-panic.conf` will reboot the machine automatically (`kernel.panic=10`, `kernel.softlockup_panic=1`).
3. **Emergency TUI**: If the dracut environment fails to read the Live block device due to faulty USB controllers, you will not be dropped to an empty `dracut#` shell. You will see the red **HisnOS CRITICAL BOOT ERROR** menu.

## Manual Hardware Debugging

If the auto-recovery mechanisms still result in a loop or you need to extract logs:

### 1. Boot 'Safe Hardware'
At the GRUB menu, select **HisnOS Safe Hardware**. 
This appends: `nomodeset nohz=off intel_pstate=passive`
- `nomodeset` forces the generic VESA/EFI framebuffer, bypassing faulty AMD/NVIDIA/Intel GPU drivers.
- `nohz=off` disables tickless kernel, mitigating deep C-state CPU bugs.

### 2. Enter Safe Mode
The Safe Hardware entry automatically mounts `hisnos-safe.target`. 
- Log in to the console. NetworkManager is running.
- Extract hardware logs:
  ```bash
  journalctl -k -b > dmesg_export.log
  journalctl -b -u display-manager > display_crash.log
  curl --upload-file ./dmesg_export.log https://transfer.sh/dmesg.txt
  ```

### 3. Rescue Shell (Extreme)
If the system cannot even reach `hisnos-safe.target`, select **HisnOS Rescue Shell** from GRUB.
This appends `rd.break` and halts the boot immediately after dracut mounts the rootfs.
- Remount `sysroot`: `mount -o remount,rw /sysroot`
- Chroot: `chroot /sysroot`
- Inspect raw filesystem integrity and `rpm-ostree` deployment logs manually.
