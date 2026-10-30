import subprocess,pathlib,json,time,hashlib,os,shutil
b=pathlib.Path('/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/tracked-delta-01');old=b.parent;final=b/'final-validation'
assert (final/'fc.done').exists() and (final/'go.done').exists(), 'final build still active'
assert all(line.endswith('\t0') for line in (final/'fc-exit.tsv').read_text().splitlines() if not line.startswith('api\t'))
assert 'cargo_toml' in (final/'fc-api.log').read_text() and 'offline' in (final/'fc-api.log').read_text()
assert all(line.endswith('\t0') for line in (final/'go-exit.tsv').read_text().splitlines())
assert shutil.disk_usage(b).free > 20*(1<<30), 'retain evidence and inspect disk before runs'
env=os.environ.copy();env.update(GOMAXPROCS='4',GOCACHE=str(old/'go-build'),GOMODCACHE='/home/coder/projects/yangshengxin/e2b/cache/go-mod',GOPROXY='off',GOSUMDB='off')
env.pop('M11_DIAGNOSTIC_SKIP_NATIVE',None)
go='/home/coder/projects/yangshengxin/e2b/toolchains/go1.25.4/bin/go'
with open(final/'fixture-build.log','w') as log:
 subprocess.run([go,'build','-p','2','-o',str(b/'page-sharing-source'),'./cmd/page-sharing-source'],env=env,check=True,cwd=b/'infra/packages/orchestrator',stdout=log,stderr=subprocess.STDOUT)
 subprocess.run([go,'build','-o',str(b/'m11-guest-init'),str(b/'infra/scripts/memory-lab/page-sharing/m11_guest.go')],env=dict(env,CGO_ENABLED='0'),check=True,cwd=b/'infra/packages/orchestrator',stdout=log,stderr=subprocess.STDOUT)
fc=b/'cargo-target/release/firecracker';scripts=b/'infra/scripts/memory-lab/page-sharing'
modules=pathlib.Path('/home/coder/tide-repro/rootfs/lib/modules/6.1.0-50-arm64/kernel/drivers/virtio');assert (modules/'virtio_balloon.ko').exists()
common=['sudo','-n','python3']
m10=common+[str(scripts/'m10_check.py'),'--firecracker',str(fc),'--kernel','/home/coder/tide-repro/assets/vmlinuz','--guest-binary',str(b/'m10-guest-init'),'--nbd-helper',str(b/'page-sharing-source'),'--module-dir',str(modules),'--tracked-delta','--cleanup-failed']
m11=common+[str(scripts/'m11_check.py'),'--firecracker',str(fc),'--kernel','/home/coder/tide-repro/assets/vmlinuz','--guest-binary',str(b/'m11-guest-init'),'--nbd-helper',str(b/'page-sharing-source'),'--module-dir',str(old/'m11-tools/modules'),'--tracked-delta','--cleanup-failed','--output',str(b/'m11-tracked-final')]
native=common+[str(scripts/'tracked_delta_check.py'),'--firecracker',str(fc),'--kernel','/home/coder/tide-repro/assets/vmlinuz','--guest-binary',str(b/'guest-init'),'--output',str(b/'native-final')]
factory=json.loads((b/'factory-tracked-01-driver.json').read_text())['command']
factory=[a.replace('factory-tracked-01','factory-tracked-final') for a in factory]
# Use the same slots only after the prior Factory exited and removed its namespaces.
ns=subprocess.check_output(['sudo','-n','ip','netns','list'],text=True)
for i in range(25500,25505):assert 'ns-'+str(i)+' ' not in ns
jobs={'discard':[('m10-balloon-tracked-final',m10+['--output',str(b/'m10-balloon-tracked-final')]),('m10-fpr-tracked-final',m10+['--free-page-reporting','--output',str(b/'m10-fpr-tracked-final')])], 'devices':[('m11-tracked-final',m11)], 'integration':[('native-final',native),('factory-tracked-final',factory)]}
for group,commands in jobs.items():
 driver=final/(group+'-driver.py')
 code="import subprocess,pathlib,json,time\nb=pathlib.Path("+repr(str(b))+ ")\njobs="+repr(commands)+"\nfor name, command in jobs:\n log=open(b/(name+'.log'),'xb')\n started=time.time();p=subprocess.Popen(command,stdout=log,stderr=subprocess.STDOUT)\n (b/(name+'-driver.json')).write_text(json.dumps({'pid':p.pid,'started':started,'command':command},indent=2))\n rc=p.wait()\n (b/(name+'-exit.json')).write_text(json.dumps({'returncode':rc,'finished':time.time()},indent=2))\n if rc:raise SystemExit(rc)\n"
 driver.write_text(code)
 log=open(final/(group+'-driver.log'),'xb');p=subprocess.Popen(['python3',str(driver)],env=env,stdout=log,stderr=subprocess.STDOUT,start_new_session=True)
 (final/(group+'-driver.json')).write_text(json.dumps({'pid':p.pid,'started':time.time(),'driver':str(driver)},indent=2));print(group,p.pid)
(final/'input-sha256.json').write_text(json.dumps({str(p):hashlib.sha256(p.read_bytes()).hexdigest() for p in [fc,b/'factory-integration.test',b/'page-sharing-source',b/'guest-init',b/'m10-guest-init',b/'m11-guest-init']},indent=2))
