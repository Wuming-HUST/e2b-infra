#!/usr/bin/env bash
# Optional openEuler 24.03 SP2 ARM64 test dependency. No system installation.
set -euo pipefail
if [[ $# != 1 ]]; then
  echo "usage: $0 NEW_OUTPUT_DIR" >&2
  exit 2
fi
test "$(uname -m)" = aarch64
output_dir=$(realpath -m "$1")
test ! -e "$output_dir"
mkdir -p "$output_dir"
cd "$output_dir"
curl --fail --location --connect-timeout 15 --max-time 90 -o iptables.rpm \
  https://repo.openeuler.openatom.cn/openEuler-24.03-LTS-SP2/update/aarch64/Packages/iptables-1.8.9-13.oe2403sp2.aarch64.rpm
echo '56be46ddd49ddbb9963d8814fb044927420735fe1cab8d3ab02a61140fb53e4e  iptables.rpm' | sha256sum -c -
rpm2cpio iptables.rpm | cpio -id --quiet
cd usr/sbin
ln -s iptables-legacy iptables
ln -s iptables-legacy-save iptables-save
ln -s iptables-legacy-restore iptables-restore
ln -s ip6tables-legacy ip6tables
ldd xtables-legacy-multi
./iptables --version
echo "Prepend to PATH for the test only: $output_dir/usr/sbin"
