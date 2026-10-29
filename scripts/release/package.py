#!/usr/bin/env python3
"""Package an already built and scanned image; never reads runtime data."""
import gzip,hashlib,io,json,os,re,shutil,subprocess,tarfile
from pathlib import Path
root=Path(__file__).resolve().parents[2]
out=Path(os.environ['RELEASE_OUTPUT']).resolve()
sha=os.environ['GITHUB_SHA']; repo=os.environ['GITHUB_REPOSITORY']; repo_id=os.environ['GITHUB_REPOSITORY_ID']
assert re.fullmatch(r'[0-9a-f]{40}',sha) and repo_id.isdecimal()
assert not out.exists() and not out.is_relative_to(root)
out.mkdir(parents=True)
def digest(p):
 with p.open('rb') as f:return hashlib.file_digest(f,'sha256').hexdigest()
def pack(path, files, compressed=False):
 with tarfile.open(path,'w:gz' if compressed else 'w') as t:
  for name,p in sorted(files.items()):
   data=p.read_bytes();info=tarfile.TarInfo(name);info.size=len(data);info.mode=0o600;info.mtime=0;t.addfile(info,io.BytesIO(data))
receiver=out/'ferry-receiver'
flags=f'-s -w -X ferry/internal/buildinfo.Version=main-{sha[:12]} -X ferry/internal/buildinfo.Commit={sha} -X ferry/internal/buildinfo.BuildTime={os.environ["BUILD_TIME"]} -X ferry/internal/buildinfo.Source=https://github.com/{repo} -X ferry/internal/receiver.RepositoryIDValue={repo_id}'
service_dir=os.environ.get('RECEIVER_SERVICE_DIR','/var/lib/ferry')
assert re.fullmatch(r'/[a-zA-Z0-9_/-]+',service_dir) and '..' not in service_dir
flags+=' -X ferry/internal/receiver.ServiceDir='+service_dir
subprocess.run(['go','build','-trimpath','-mod=readonly','-ldflags',flags,'-o',str(receiver),'./cmd/ferry-receiver'],cwd=root,env=dict(os.environ,CGO_ENABLED='0',GOOS='linux',GOARCH='amd64'),check=True)
subprocess.run(['docker','save','ferry:'+sha,'-o',str(out/'image.tar')],check=True)
with (out/'image.tar').open('rb') as src, gzip.open(out/'image.tar.gz','wb',compresslevel=1) as dest:shutil.copyfileobj(src,dest)
(out/'image.tar').unlink()
files={p.name:p for p in (root/'deploy/release').iterdir() if p.name in ['compose.yml','worker-README.md','bootstrap-config.template.yaml']}
assert len(files)==3
pack(out/'release-bundle.tar.gz',files,True)
payload={'schema':'ferry/release-payload/v1','release_sha':sha,'files':[{'name':n,'size':(out/n).stat().st_size,'sha256':digest(out/n)} for n in ['image.tar.gz','release-bundle.tar.gz']]}
(out/'manifest.json').write_text(json.dumps(payload)+'\n')
pack(out/'payload.tar',{n:out/n for n in ['image.tar.gz','release-bundle.tar.gz','manifest.json']})
for n in ['image.tar.gz','release-bundle.tar.gz','manifest.json']:(out/n).unlink()
shutil.copyfile(root/'scripts/release/verify_deployment.sh',out/'verify_deployment.sh')
manifest={'schema':'ferry/release/v1','repository':repo,'repository_id':repo_id,'commit':sha,'workflow_run_id':os.environ['GITHUB_RUN_ID'],'workflow_run_attempt':os.environ['GITHUB_RUN_ATTEMPT'],'image_digest':os.environ['IMAGE_DIGEST'],'files':{p.name:digest(p) for p in out.iterdir() if p.is_file()}}
(out/'release.json').write_text(json.dumps(manifest,indent=2)+'\n')
print(json.dumps({'commit':sha,'image_digest':manifest['image_digest'],'files':list(manifest['files'])}))
