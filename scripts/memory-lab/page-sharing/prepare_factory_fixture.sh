#!/usr/bin/env bash
# Build a disposable Factory fixture from the existing ARM64 envd image.
# Never alter the input image, initrd tree, or host package installation.
set -euo pipefail
if [[ $# != 2 ]]; then
  echo "usage: $0 EXISTING_TEMPLATE_DIR NEW_OUTPUT_DIR" >&2
  exit 2
fi
source_dir=$(realpath "$1")
output_dir=$(realpath -m "$2")
test ! -e "$output_dir"
mkdir -p "$output_dir"
cp --reflink=auto --sparse=always "$source_dir/rootfs.ext4" "$output_dir/rootfs.ext4"
sudo -n cp -a "$source_dir/initrd-tree" "$output_dir/initrd-tree"
cat > "$output_dir/sharing-init" <<'GUEST'
#!/bin/sh
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
mount -t tmpfs tmpfs /run
mkdir -p /run/e2b /workspace /sys/fs/cgroup /dev/pts
mount -t devpts devpts /dev/pts
mount -t cgroup2 none /sys/fs/cgroup || true
echo "+cpu +memory" > /sys/fs/cgroup/cgroup.subtree_control || true
MD=/lib/modules/6.1.0-50-arm64/kernel
for m in net/core/failover drivers/net/net_failover drivers/net/virtio_net; do
  insmod "$MD/$m.ko" 2>/dev/null || true
done
ip link set lo up
IP=
for arg in $(cat /proc/cmdline); do
  case "$arg" in ip=*) IP=${arg#ip=}; IP=${IP%%:*};; esac
done
test -n "$IP" || { echo SHARING_INIT_MISSING_IP; exec sh; }
ip link set eth0 up
ip addr add "$IP/30" dev eth0
# The driver is a module, so the kernel's early ip= autoconfiguration cannot
# establish this route. Factory's namespace DNAT retains the host source IP.
ip route replace default via 169.254.0.22 dev eth0
ulimit -n 8192
# Only MMDS/log transport differs; process/filesystem APIs are real envd.
/usr/local/bin/envd -isnotfc > /dev/console 2>&1 &
echo "SHARING_INIT_READY ip=$IP envd=$!"
while true; do wait; sleep 1; done
GUEST
chmod 755 "$output_dir/sharing-init"
debugfs -w -R "write $output_dir/sharing-init /sbin/sharing-init" "$output_dir/rootfs.ext4"
debugfs -w -R 'set_inode_field /sbin/sharing-init mode 0100755' "$output_dir/rootfs.ext4"
debugfs -R "dump /sbin/sharing-init $output_dir/sharing-init-readback" "$output_dir/rootfs.ext4"
cmp "$output_dir/sharing-init" "$output_dir/sharing-init-readback"
sudo -n python3 - "$output_dir/initrd-tree/init" <<'PY'
from pathlib import Path
import sys
p = Path(sys.argv[1])
text = p.read_text()
assert '/sbin/memory-lab-init' in text
p.write_text(text.replace('/sbin/memory-lab-init', '/sbin/sharing-init'))
PY
(cd "$output_dir/initrd-tree"; sudo -n find . -print0 | sudo -n cpio --null -o --format=newc --quiet | gzip -n > "$output_dir/initrd.gz")
sha256sum "$source_dir/rootfs.ext4" "$source_dir/initrd.gz" "$output_dir/rootfs.ext4" "$output_dir/initrd.gz" "$output_dir/sharing-init" > "$output_dir/sha256.txt"
echo "Fixture ready: $output_dir"
