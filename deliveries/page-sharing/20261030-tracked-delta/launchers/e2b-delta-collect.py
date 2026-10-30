import pathlib,json,hashlib,tarfile,time
b=pathlib.Path('/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/tracked-delta-01');files=set()
# Receipts and logs only. Never archive RAM, disk images, caches or binaries.
for d in [b,b/'final-validation',b/'attempt-01',b/'attempt-02']:
 if d.exists():
  for p in d.iterdir():
   if p.is_file() and p.suffix in ['.json','.log','.tsv','.done','.sh','.py'] and p.stat().st_size<3_000_000:files.add(p)
for name in ['native-final','factory-tracked-final','m10-balloon-tracked-final','m10-fpr-tracked-final','m11-tracked-final','native-01','factory-tracked-01']:
 d=b/name
 if not d.exists():continue
 for p in d.rglob('*'):
  if not p.is_file() or p.stat().st_size>=3_000_000:continue
  if p.suffix not in ['.json','.jsonl','.log','.tsv','.txt','.csv']:continue
  rel=p.relative_to(d)
  if len(rel.parts)>2:continue
  files.add(p)
# Large artifacts remain on the node; record size and digest for replay inputs/outputs.
large=[]
for name in ['native-final','factory-tracked-final','m10-balloon-tracked-final','m10-fpr-tracked-final','m11-tracked-final']:
 for p in (b/name).iterdir():
  if p.is_file() and p.suffix in ['.ram','.snap','.disk']:
   large.append({'path':str(p),'size':p.stat().st_size,'sha256':hashlib.file_digest(open(p,'rb'),'sha256').hexdigest()})
f=b/'final-validation/retained-large-artifacts.json';f.write_text(json.dumps(large,indent=2));files.add(f)
manifest={str(p.relative_to(b)):hashlib.file_digest(open(p,'rb'),'sha256').hexdigest() for p in sorted(files)}
m=b/'receipt-sha256.json';m.write_text(json.dumps(manifest,indent=2));files.add(m)
t=b/'tracked-evidence.tar.gz'
with tarfile.open(t,'w:gz') as out:
 for p in sorted(files):out.add(p,arcname=str(p.relative_to(b)),recursive=False)
print(json.dumps({'path':str(t),'size':t.stat().st_size,'sha256':hashlib.file_digest(open(t,'rb'),'sha256').hexdigest(),'files':len(files),'large_artifacts':len(large)}))
