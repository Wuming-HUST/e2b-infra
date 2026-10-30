import subprocess,pathlib,json
b=pathlib.Path('/home/coder/projects/yangshengxin/e2b/page-sharing-20261030/tracked-delta-01')
for n in ['audit','collect']:
 with open(b/'final-validation'/('delivery-'+n+'.log'),'w') as out: subprocess.run(['sudo','-n','python3',str(b/'final-validation'/('delivery-'+n+'.py'))],stdout=out,stderr=subprocess.STDOUT,check=True)
(b/'final-validation/delivery.done').write_text('complete')
