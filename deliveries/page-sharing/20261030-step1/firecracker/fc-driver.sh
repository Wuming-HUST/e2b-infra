#!/bin/sh
set -u
task_root=/home/coder/projects/yangshengxin/e2b/page-sharing-20261030
cache_src=/home/coder/projects/yangshengxin/e2b/validation-20261016/toolchains/cargo
toolchain=/home/coder/projects/yangshengxin/e2b/validation-20261016/toolchains/rustup/toolchains/1.85.0-aarch64-unknown-linux-gnu
printf '%s\n' "$$" > "$task_root/fc-driver.pid"
mkdir -p "$task_root/cargo-home"
if [ ! -d "$task_root/cargo-home/git" ]; then cp -a "$cache_src/git" "$task_root/cargo-home/" || exit 90; fi
if [ ! -d "$task_root/cargo-home/registry" ]; then cp -a "$cache_src/registry" "$task_root/cargo-home/" || exit 91; fi
export CARGO_HOME="$task_root/cargo-home"
export CARGO_TARGET_DIR="$task_root/cargo-target"
export CARGO_BUILD_JOBS=4
if "$toolchain/bin/rustc" --version >/dev/null 2>&1; then
    export PATH="$toolchain/bin:/usr/local/bin:/usr/bin:/bin"
    export RUSTC="$toolchain/bin/rustc"
    export RUSTDOC="$toolchain/bin/rustdoc"
else
    export PATH=/usr/local/bin:/usr/bin:/bin
    export RUSTC=/usr/bin/rustc
    export RUSTDOC=/usr/bin/rustdoc
fi
cd "$task_root/firecracker" || exit 92
{ cargo --version; "$RUSTC" --version; printf 'CARGO_HOME=%s\nCARGO_TARGET_DIR=%s\nCARGO_BUILD_JOBS=%s\n' "$CARGO_HOME" "$CARGO_TARGET_DIR" "$CARGO_BUILD_JOBS"; } > "$task_root/fc-toolchain.log" 2>&1
cargo fmt --all -- --check > "$task_root/fc-fmt.log" 2>&1
fmt_status=$?
printf 'fmt\t%s\n' "$fmt_status" > "$task_root/fc-exit-codes.tsv"
if [ "$fmt_status" -ne 0 ]; then
    python3 - "$task_root" <<'PY'
import hashlib,json,os,sys
base=sys.argv[1]
hashes={}
for root,dirs,files in os.walk(base+'/firecracker'):
    dirs[:]=[d for d in dirs if d not in {'.git','build','target'}]
    for name in files:
        if name.endswith('.rs'):
            path=os.path.join(root,name)
            hashes[os.path.relpath(path,base+'/firecracker')]=hashlib.sha256(open(path,'rb').read()).hexdigest()
json.dump(hashes,open(base+'/fc-format-before.json','w'),indent=2)
PY
    cargo fmt --all > "$task_root/fc-fmt-apply.log" 2>&1
    printf 'fmt-apply\t%s\n' "$?" >> "$task_root/fc-exit-codes.tsv"
    python3 - "$task_root" <<'PY'
import hashlib,json,os,sys,tarfile
base=sys.argv[1]
before=json.load(open(base+'/fc-format-before.json'))
changed=[]
for path,digest in before.items():
    current=hashlib.sha256(open(base+'/firecracker/'+path,'rb').read()).hexdigest()
    if current!=digest:changed.append(path)
json.dump(changed,open(base+'/fc-formatted-files.json','w'),indent=2)
with tarfile.open(base+'/fc-formatted-sources.tar.gz','w:gz') as archive:
    for path in changed:archive.add(base+'/firecracker/'+path,arcname=path)
PY
    cargo fmt --all -- --check > "$task_root/fc-fmt-after.log" 2>&1
    printf 'fmt-after\t%s\n' "$?" >> "$task_root/fc-exit-codes.tsv"
fi
cargo test --offline --locked -p vmm test_external_snapshot -- --nocapture > "$task_root/fc-tests.log" 2>&1
printf 'tests\t%s\n' "$?" >> "$task_root/fc-exit-codes.tsv"
cargo build --release --offline --locked -p firecracker > "$task_root/fc-build.log" 2>&1
printf 'build\t%s\n' "$?" >> "$task_root/fc-exit-codes.tsv"
date -u '+%Y-%m-%dT%H:%M:%SZ' > "$task_root/fc-driver.done"
