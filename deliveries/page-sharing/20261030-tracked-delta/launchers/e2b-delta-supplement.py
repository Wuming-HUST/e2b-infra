import os,pathlib,json,hashlib,shutil,subprocess,time
b=pathlib.Path('/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/tracked-delta-01');f=b/'final-validation'
cache=next((b.parent/'cargo-home/registry/cache').iterdir());records=[]
for x in json.loads((b/'dev-crates/manifest.json').read_text()):
 p=b/'dev-crates'/(x['name']+'-'+x['version']+'.crate');assert hashlib.sha256(p.read_bytes()).hexdigest()==x['checksum'];target=cache/p.name
 if target.exists():assert hashlib.sha256(target.read_bytes()).hexdigest()==x['checksum']
 else:shutil.copyfile(p,target)
 records.append({'file':str(target),'sha256':x['checksum']})
(f/'dependency-api-dev.json').write_text(json.dumps(records,indent=2))
tool=pathlib.Path('/home/coder/projects/yangshengxin/e2b/validation-20261016/toolchains/rustup/toolchains/1.85.0-aarch64-unknown-linux-gnu')
env=os.environ.copy();env.update(CARGO_HOME=str(b.parent/'cargo-home'),CARGO_TARGET_DIR=str(b/'cargo-target'),CARGO_BUILD_JOBS='4',RUSTC=str(tool/'bin/rustc'),RUSTDOC=str(tool/'bin/rustdoc'),PATH=str(tool/'bin')+':'+env['PATH'],LIBRARY_PATH='/home/coder/projects/yangshengxin/e2b/validation-20261016/toolchains/lib',GOMAXPROCS='4',GOCACHE=str(b.parent/'go-build'),GOMODCACHE='/home/coder/projects/yangshengxin/e2b/cache/go-mod',GOPROXY='off',GOSUMDB='off')
exe=next(p for p in (b/'cargo-target/debug/deps').glob('vmm-*') if p.is_file() and p.suffix=='' and os.access(p,os.X_OK))
files=['src/vmm/src/'+x for x in ['lib.rs','builder.rs','persist.rs','rpc_interface.rs','vstate/vm.rs','vmm_config/instance_info.rs','devices/virtio/balloon/util.rs','utils/dirty_tracking.rs']]+['src/firecracker/src/api_server/parsed_request.rs']
jobs=[('api-complete',[str(tool/'bin/cargo'),'test','--offline','--locked','-p','firecracker','api_server::parsed_request','--','--nocapture'],b/'fc'),('fmt-check',[str(tool/'bin/rustfmt'),'--check','--edition','2024','--config','skip_children=true']+files,b/'fc'),('freeze-regressions',['sudo','-n',str(exe),'test_external_snapshot','--nocapture'],b/'fc'),('prepare-save',['sudo','-n',str(exe),'devices::virtio::block::virtio::device::tests::test_prepare_save','--nocapture'],b/'fc'),('headers-full',['/home/coder/projects/yangshengxin/e2b/toolchains/go1.25.4/bin/go','test','-p','2','../shared/pkg/storage/header','./internal/template/metadata','-count=1','-v'],b/'infra/packages/orchestrator')]
for name,cmd,cwd in jobs:
 log=f/(name+'.log');start=time.time()
 with log.open('w') as stream:r=subprocess.run(cmd,cwd=cwd,env=env,stdout=stream,stderr=subprocess.STDOUT)
 (f/(name+'-exit.json')).write_text(json.dumps({'returncode':r.returncode,'seconds':time.time()-start,'command':cmd},indent=2))
(f/'supplement.done').write_text('complete')
