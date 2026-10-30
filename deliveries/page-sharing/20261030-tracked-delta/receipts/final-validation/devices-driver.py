import subprocess,pathlib,json,time
b=pathlib.Path('/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/tracked-delta-01')
jobs=[('m11-tracked-final', ['sudo', '-n', 'python3', '/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/tracked-delta-01/infra/scripts/memory-lab/page-sharing/m11_check.py', '--firecracker', '/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/tracked-delta-01/cargo-target/release/firecracker', '--kernel', '/home/coder/tide-repro/assets/vmlinuz', '--guest-binary', '/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/tracked-delta-01/m11-guest-init', '--nbd-helper', '/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/tracked-delta-01/page-sharing-source', '--module-dir', '/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/m11-tools/modules', '--tracked-delta', '--cleanup-failed', '--output', '/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/tracked-delta-01/m11-tracked-final'])]
for name, command in jobs:
 log=open(b/(name+'.log'),'xb')
 started=time.time();p=subprocess.Popen(command,stdout=log,stderr=subprocess.STDOUT)
 (b/(name+'-driver.json')).write_text(json.dumps({'pid':p.pid,'started':started,'command':command},indent=2))
 rc=p.wait()
 (b/(name+'-exit.json')).write_text(json.dumps({'returncode':rc,'finished':time.time()},indent=2))
 if rc:raise SystemExit(rc)
