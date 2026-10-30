import pathlib,shutil,subprocess,json,time
b=pathlib.Path('/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/tracked-delta-01')
assert (b/'fc.done').exists() and (b/'go.done').exists()
previous=b/'attempt-02';previous.mkdir()
for p in b.iterdir():
 if p.is_file() and (p.suffix in ('.log','.tsv','.done','.json','.sh')):shutil.copy2(p,previous/p.name)
shutil.copy2(b/'cargo-target/release/firecracker',previous/'firecracker')
shutil.copy2(b/'factory-integration.test',previous/'factory-integration.test')
final=b/'final-validation';final.mkdir()
for kind in ('fc','go'):
 script=(b/(kind+'-driver.sh')).read_text()
 # Only redirect logs/results, keeping source and new binaries at their own paths.
 import re
 script=re.sub(r'\$base/([a-zA-Z0-9-]+\.(?:log|tsv|done))',r'$base/final-validation/\1',script)
 if kind=='fc':
  script=script.replace('src/vmm/src/lib.rs src/vmm/src/builder.rs','src/vmm/src/vstate/vm.rs src/vmm/src/lib.rs src/vmm/src/builder.rs')
  script=script.replace('discard_marks_dirty_without_a_cpu_write','devices::virtio::balloon::util')
  addition='''cargo test --offline --locked -p vmm devices::virtio::iovec -- --nocapture > "$base/final-validation/fc-iovec.log" 2>&1
printf 'iovec\\t%s\\n' "$?" >> "$base/final-validation/fc-exit.tsv"
cargo test --offline --locked -p firecracker api_server::parsed_request -- --nocapture > "$base/final-validation/fc-api.log" 2>&1
printf 'api\\t%s\\n' "$?" >> "$base/final-validation/fc-exit.tsv"
'''
  script=script.replace('cargo build --release',addition+'cargo build --release')
 path=final/(kind+'-driver.sh');path.write_text(script)
 log=open(final/(kind+'-driver.log'),'xb');p=subprocess.Popen(['sh',str(path)],stdout=log,stderr=subprocess.STDOUT,start_new_session=True)
 rec={'pid':p.pid,'started':time.time(),'driver':str(path)};(final/(kind+'-driver.json')).write_text(json.dumps(rec,indent=2));print(kind,json.dumps(rec))
