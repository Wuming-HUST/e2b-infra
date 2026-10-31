#!/usr/bin/env python3
"""Copy current test sources into a new target campaign, preserving older runs."""
from pathlib import Path
import argparse
import json
import subprocess
import tarfile

parser = argparse.ArgumentParser()
parser.add_argument('suffix')
parser.add_argument('--slot', required=True, type=int)
parser.add_argument('--modes', default='full_logical,tracked_delta')
args = parser.parse_args()
assert args.suffix.isdecimal() and 1 <= args.slot < 32000
root = Path(__file__).resolve().parents[4]
files = ['packages/orchestrator/internal/sandbox/pmem_compat_integration_linux_test.go',
         'packages/orchestrator/internal/sandbox/testdata/pmem_compat.py',
         'packages/orchestrator/internal/sandbox/testdata/pmem_overlay_baseline.py',
         'deliveries/page-sharing/20261031-remaining-tests/reproduce/run-compat-inbound.py']
archive = Path('/tmp/e2b-compat-' + args.suffix + '.tgz')
with tarfile.open(archive, 'w:gz') as stream:
    for name in files:
        stream.add(root / name, arcname=name)
ssh = ['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10', 'H-arch']
inner = 'ssh -o BatchMode=yes -o ConnectTimeout=10 agent03.coder '
with archive.open('rb') as stream:
    subprocess.run(ssh + [inner + '"cat > ' + str(archive) + '"'], stdin=stream, check=True)
script = '''from pathlib import Path
import hashlib,json,subprocess,os
b=Path('/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/pmem-dax-01')
d=b/('remaining-20261031-'+SUFFIX);d.mkdir()
subprocess.run(['cp','-a','--reflink=auto',str(b/'remaining-20261031-04/infra'),str(d/'infra')],check=True)
subprocess.run(['tar','-xzf',ARCHIVE,'-C',str(d/'infra')],check=True)
manifest={name:hashlib.sha256((d/'infra'/name).read_bytes()).hexdigest() for name in FILES}
(d/'test-source-sha256.json').write_text(json.dumps(manifest,indent=2)+'\\n')
script=d/'infra/deliveries/page-sharing/20261031-remaining-tests/reproduce/run-compat-inbound.py'
env=dict(os.environ,E2B_COMPAT_CAMPAIGN_SUFFIX=SUFFIX,E2B_COMPAT_SLOT_BASE=SLOT,E2B_COMPAT_MODES=MODES)
with (d/'driver.log').open('x') as log:p=subprocess.Popen(['python3',str(script)],env=env,stdin=subprocess.DEVNULL,stdout=log,stderr=subprocess.STDOUT,start_new_session=True)
(d/'campaign-driver.json').write_text(json.dumps({'pid':p.pid,'script':str(script)},indent=2)+'\\n')
print(json.dumps({'directory':str(d),'pid':p.pid}))
'''
prefix = '\n'.join(name + '=' + repr(value) for name, value in
                   [('SUFFIX', args.suffix), ('ARCHIVE', str(archive)), ('FILES', files),
                    ('SLOT', str(args.slot)), ('MODES', args.modes)]) + '\n'
subprocess.run(ssh + [inner + '"sudo -n python3 -"'], input=prefix + script, text=True, check=True)
