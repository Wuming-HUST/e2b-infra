set -eu
cd /home/coder/projects/yangshengxin/e2b/page-sharing-20261030/firecracker
export PATH=/home/coder/projects/yangshengxin/e2b/validation-20261016/toolchains/rustup/toolchains/1.85.0-aarch64-unknown-linux-gnu/bin:$PATH
export CARGO_HOME=/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/cargo-home
export CARGO_TARGET_DIR=/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/m11-cargo-target
export LIBRARY_PATH=/home/coder/projects/yangshengxin/e2b/validation-20261016/toolchains/lib
python3 - <<'TOUCH'
from pathlib import Path
for path in Path('src/vmm/src').rglob('*.rs'):
    if not path.name.startswith('._'):path.touch()
TOUCH
sha256sum -c ../m11-fc-all-source-sha256.txt > ../m11-source-match-final.log
rustfmt --edition 2021 --config skip_children=true --check src/vmm/src/device_manager/mmio.rs src/vmm/src/device_manager/persist.rs src/vmm/src/lib.rs src/vmm/src/persist.rs src/vmm/src/devices/virtio/block/device.rs src/vmm/src/devices/virtio/block/virtio/device.rs src/vmm/src/devices/virtio/vsock/device.rs > ../m11-fc-fmt-final.log 2>&1
cargo test --offline -p vmm test_checkpoint_ --no-run --message-format=json > ../m11-fc-contract-final-build.jsonl 2> ../m11-fc-contract-final-build.log
python3 - <<'INNER'
import json
from pathlib import Path
items=[json.loads(line) for line in Path('../m11-fc-contract-final-build.jsonl').read_text().splitlines() if line.startswith('{')]
executables=[v['executable'] for v in items if v.get('reason')=='compiler-artifact' and v.get('profile',{}).get('test') and v.get('executable') and v.get('target',{}).get('name')=='vmm']
assert len(executables)==1,executables
Path('../m11-test-final-binary.path').write_text(executables[0]+'\n')
INNER
read -r test_binary < ../m11-test-final-binary.path
sudo -n "$test_binary" test_checkpoint_ --nocapture > ../m11-fc-contract-final-test.log 2>&1
sudo -n "$test_binary" test_prepare_save --nocapture > ../m11-fc-prepare-save-regression.log 2>&1
sudo -n "$test_binary" test_external_snapshot --nocapture > ../m11-fc-external-snapshot-regression.log 2>&1
sudo -n "$test_binary" devices::virtio::block::virtio::event_handler::tests::test_event_handler --exact --nocapture > ../m11-fc-block-event-regression.log 2>&1
cargo build --offline --release -p firecracker -j 4 > ../m11-fc-source-matched-build.log 2>&1
cp ../m11-cargo-target/release/firecracker ../m11-tools/firecracker-source-matched
sha256sum ../m11-tools/firecracker-source-matched > ../m11-fc-source-matched-sha256.txt
cd ..
sudo -n python3 m11-tools/scripts/memory-lab/page-sharing/m11_check.py --firecracker m11-tools/firecracker-source-matched --kernel /home/coder/tide-repro/assets/vmlinuz --guest-binary m11-tools/m11-guest --nbd-helper software-delivery/page-sharing-source --module-dir m11-tools/modules --output m11-source-matched-final --cleanup-failed > m11-source-matched-final.log 2>&1
