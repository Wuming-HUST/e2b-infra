#!/usr/bin/env bash
# Offline conversion: original input remains untouched. Never run on an active image.
set -euo pipefail
if [[ $# != 3 ]]; then echo "usage: $0 QUIESCENT_ROOTFS_EXT4 STATIC_PMEM_INIT NEW_OUTPUT_DIR" >&2; exit 2; fi
source_image=$(realpath "$1")
init_binary=$(realpath "$2")
output=$(realpath -m "$3")
test ! -e "$output"
test -f "$source_image"
test -x "$init_binary"
# Refuse an inconsistent source; repair only the disposable destination copy.
e2fsck -f -n "$source_image"
mkdir -m 700 "$output"
cp --reflink=auto --sparse=always "$source_image" "$output/lower.raw"
python3 - "$output/lower.raw" <<'PY'
import pathlib,sys
p=pathlib.Path(sys.argv[1]);a=128<<20
with p.open('r+b') as f:f.truncate((p.stat().st_size+a-1)//a*a)
PY
# Optional deterministic filesystem fixture, confined to this new lower copy.
if [[ ${E2B_PMEM_ADD_FIXTURE:-0} == 1 ]]; then
  printf 'immutable-lower-fixture\n' > "$output/lower-fixture.txt"
  debugfs -w -R 'mkdir /pmem-fixture' "$output/lower.raw"
  debugfs -w -R 'mkdir /pmem-fixture/opaque' "$output/lower.raw"
  debugfs -w -R "write $output/lower-fixture.txt /pmem-fixture/original" "$output/lower.raw"
  debugfs -w -R "write $output/lower-fixture.txt /pmem-fixture/opaque/lower" "$output/lower.raw"
fi
tune2fs -O ^has_journal -L E2BLOWER "$output/lower.raw"
set +e
e2fsck -f -p "$output/lower.raw"
rc=$?
set -e
((rc<=1))
e2fsck -f -n "$output/lower.raw"
# Initialize PFN_MODE_RAM metadata offline. The guest must not initialize
# struct pages or namespace metadata in the immutable shared backing.
mv "$output/lower.raw" "$output/lower.ext4"
python3 "$(dirname "$0")/pack-pfn.py" "$output/lower.ext4" "$output/lower.raw"
mkdir -p "$output/upper-tree/upper" "$output/upper-tree/work" "$output/initrd-tree"
truncate -s 128M "$output/upper.ext4"
mkfs.ext4 -q -F -b 4096 -L E2BUPPER -d "$output/upper-tree" "$output/upper.ext4"
cp "$init_binary" "$output/initrd-tree/init"
chmod 755 "$output/initrd-tree/init"
python3 "$(dirname "$0")/make-initramfs.py" "$output/initrd-tree/init" "$output/initrd.gz"
chmod 444 "$output/lower.raw"
python3 - "$source_image" "$output" <<'PY'
import hashlib,json,pathlib,sys
source=pathlib.Path(sys.argv[1]);out=pathlib.Path(sys.argv[2])
def sha(p):
 with p.open('rb') as f:return hashlib.file_digest(f,'sha256').hexdigest()
layout={'format_version':1,'mode':'pmem-dax-overlay','lower':{'sha256':sha(out/'lower.raw'),'size':(out/'lower.raw').stat().st_size},'guest_address':1<<38,'data_offset':2<<20}
(out/'layout.json').write_text(json.dumps(layout,indent=2)+'\n')
(out/'source.json').write_text(json.dumps({'source_path':str(source),'source_sha256':sha(source),'conversion':'block-preserving copy; offline journal removal; PFN_MODE_RAM v1.4 with 2MiB data offset; padded to 128MiB'},indent=2)+'\n')
PY
