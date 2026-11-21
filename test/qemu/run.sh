#!/bin/sh
# Boot a kernel in QEMU with a minimal initramfs that loads and unloads the
# given module, printing the kernel log on the serial console. Prints
# "TEST: PASS" / "TEST: FAIL" at the end.
#
# Usage: test/qemu/run.sh path/to/module.ko [param=value ...]
#
#   TEST_PROG=dir               Go main package (built with the regular Go
#                               toolchain) to run in the VM after loading
#   EXPECT_LOAD_ERRNO=ENODEV    expect loading to fail with this errno
#   SMP=2                       number of CPUs of the VM
#   MEM=512                     memory of the VM in MiB
#   KERNEL=/boot/vmlinuz-linux  kernel image to boot (must match the kernel
#                               the module was built for)
#   EXTRA_MODULES="drivers/block/brd.ko.zst ..."
#                               modules of the booted kernel (paths under
#                               /lib/modules/<version>/kernel) copied to
#                               /extra/ in the VM, for TEST_PROG to load
set -eu

KO=$(realpath "$1")
shift
HERE=$(cd "$(dirname "$0")" && pwd)
KERNEL=${KERNEL:-/boot/vmlinuz-linux}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$WORK/root/proc" "$WORK/root/dev"
(cd "$HERE" && CGO_ENABLED=0 go build -o "$WORK/root/init" ./init)
cp "$KO" "$WORK/root/module.ko"
echo "$*" >"$WORK/root/params"
if [ -n "${TEST_PROG:-}" ]; then
	(cd "$TEST_PROG" && CGO_ENABLED=0 GOWORK=off go build -o "$WORK/root/test" .)
fi
if [ -n "${EXTRA_MODULES:-}" ]; then
	KVER=$(file -bL "$KERNEL" | sed -n 's/.*version \([^ ]*\).*/\1/p')
	mkdir -p "$WORK/root/extra"
	for m in $EXTRA_MODULES; do
		if grep -qx "kernel/${m%.zst}" "/lib/modules/$KVER/modules.builtin"; then
			continue
		fi
		cp "/lib/modules/$KVER/kernel/$m" "$WORK/root/extra/"
	done
fi
if [ -n "${EXPECT_LOAD_ERRNO:-}" ]; then
	echo "$EXPECT_LOAD_ERRNO" >"$WORK/root/expect_errno"
fi
(cd "$WORK/root" && find . | cpio -o -H newc --quiet | gzip -1) >"$WORK/initramfs.cpio.gz"

ACCEL="-machine accel=tcg"
if [ -w /dev/kvm ]; then
	ACCEL="-enable-kvm -cpu host"
fi

# panic=-1 + -no-reboot: a kernel panic exits QEMU instead of hanging.
timeout 120 qemu-system-x86_64 $ACCEL -m ${MEM:-512} -smp ${SMP:-2} \
	-kernel "$KERNEL" -initrd "$WORK/initramfs.cpio.gz" \
	-append "console=ttyS0 loglevel=7 panic=-1 oops=panic" \
	-nographic -no-reboot
