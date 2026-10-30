import pathlib,json,hashlib,os,subprocess,re,time
b=pathlib.Path('/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/tracked-delta-01');f=b/'final-validation'
r={}
for n in ['native-final','factory-tracked-final','m10-balloon-tracked-final','m10-fpr-tracked-final','m11-tracked-final']:
 d=b/n;entries={}
 for p in sorted(d.glob('*.json')):
  try:entries[p.name]=json.loads(p.read_text())
  except Exception:pass
 r[n]={'exit':json.loads((b/(n+'-exit.json')).read_text()),'receipts':entries}
# Only inspect PIDs recorded by these runs; never kill by global process name.
pids=[]
for n in r:
 for p in (b/n).rglob('*pid*.json'):
  try:
   data=json.loads(p.read_text())
   def check(x,key=''):
    if isinstance(x,dict):
     for k,v in x.items():check(v,k)
    elif isinstance(x,list):
     for v in x:check(v,key)
    elif isinstance(x,int) and key in ['pid','PID']:
     proc=pathlib.Path('/proc')/str(x);cmd=(proc/'cmdline').read_bytes().replace(b'\0',b' ').decode(errors='replace') if proc.exists() else ''
     pids.append({'receipt':str(p.relative_to(b)),'pid':x,'exists':proc.exists(),'cmdline':cmd})
   check(data)
  except (OSError,ValueError):pass
r['pid_audit']=pids
r['namespaces']=subprocess.check_output(['ip','netns','list'],text=True)
r['original_rootfs']={'path':str(b.parent/'factory-fixture-v2/rootfs.ext4'),'sha256':hashlib.file_digest(open(b.parent/'factory-fixture-v2/rootfs.ext4','rb'),'sha256').hexdigest()}
r['tests']={}
for p in f.glob('*-exit.json'):r['tests'][p.name]=json.loads(p.read_text())
for n in ['fc-exit.tsv','go-exit.tsv']:r['tests'][n]=(f/n).read_text()
r['test_counts']={p.name:{'passed_lines':len(re.findall(r'^--- PASS:',p.read_text(errors='replace'),re.M)),'result_lines':re.findall(r'^test result:.*$',p.read_text(errors='replace'),re.M)} for p in f.glob('*.log') if p.stat().st_size<3_000_000}
(f/'audit.json').write_text(json.dumps(r,indent=2));print('AUDIT_JSON='+json.dumps(r))
