#!/usr/bin/env python3
"""Build local-only candidate binaries with input hashes; does not publish releases."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
TARGETS = ('darwin/arm64', 'linux/amd64', 'linux/arm64')

def notice_paths():
    return sorted([ROOT/'LICENSE', ROOT/'NOTICE',
                   *[p for p in (ROOT/'LICENSES').rglob('*') if p.is_file()],
                   *[ROOT/'docs'/name for name in (
                       'third-party-notices.md', 'container-notices.md',
                       'runtime-dependencies.json', 'toolchain-notices.json',
                       'asset-provenance.md')]])

def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()

def objects(text):
    decoder = json.JSONDecoder()
    while text.strip():
        text = text.lstrip()
        value, end = decoder.raw_decode(text)
        yield value
        text = text[end:]

def run(*args, env=None):
    return subprocess.check_output(args, cwd=ROOT, env=env, text=True).strip()

def environment(target):
    system, arch = target.split('/')
    return dict(os.environ, GOOS=system, GOARCH=arch, CGO_ENABLED='0', GOWORK='off')

def inputs():
    paths = {ROOT/'go.mod', ROOT/'go.sum', Path(__file__).resolve()}
    paths.update(notice_paths())
    for target in TARGETS:
        for package in objects(run('go', 'list', '-deps', '-json', './cmd/ferry', env=environment(target))):
            directory = Path(package['Dir'])
            if not directory.is_relative_to(ROOT):
                continue
            for field in ('GoFiles', 'CgoFiles', 'SFiles', 'SysoFiles', 'EmbedFiles'):
                paths.update(directory/name for name in package.get(field, []))
    return {p.relative_to(ROOT).as_posix():digest(p) for p in sorted(paths)}

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('output', type=Path, help='new directory outside the source tree')
    args = parser.parse_args()
    output = args.output.resolve()
    if output.exists() or output.is_relative_to(ROOT):
        parser.error('output must be a new directory outside the source tree')
    subprocess.run(['python3', str(ROOT/'scripts/check_notices.py')], cwd=ROOT, check=True)
    source = inputs()
    fingerprint = hashlib.sha256(json.dumps(source, sort_keys=True, separators=(',', ':')).encode()).hexdigest()
    version = 'candidate-'+fingerprint[:16]
    flags = f'-s -w -X ferry/internal/buildinfo.Version={version} -X ferry/internal/buildinfo.Commit=unknown -X ferry/internal/buildinfo.BuildTime=unknown -X ferry/internal/buildinfo.Source=local-candidate'
    output.parent.mkdir(parents=True, exist_ok=True)
    artifacts = []
    with tempfile.TemporaryDirectory(prefix='ferry-build-', dir=output.parent) as temporary:
        staging = Path(temporary)
        notices = {}
        for path in notice_paths():
            relative = path.relative_to(ROOT)
            destination = staging/relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(path, destination)
            notices[relative.as_posix()] = digest(destination)
        for target in TARGETS:
            binary = staging/('ferry-'+target.replace('/', '-'))
            subprocess.run(['go', 'build', '-mod=readonly', '-buildvcs=false', '-trimpath',
                            '-ldflags', flags, '-o', str(binary), './cmd/ferry'],
                           cwd=ROOT, env=environment(target), check=True)
            buildinfo = run('go', 'version', '-m', str(binary))
            system, arch = target.split('/')
            for expected in ('GOOS='+system, 'GOARCH='+arch, 'CGO_ENABLED=0'):
                if expected not in buildinfo:
                    raise ValueError('binary metadata missing: '+expected)
            if version.encode() not in binary.read_bytes():
                raise ValueError('candidate version missing from binary')
            if target == run('go', 'env', 'GOHOSTOS')+'/'+run('go', 'env', 'GOHOSTARCH'):
                identity = json.loads(subprocess.check_output([str(binary), '-version-json']))
                if identity['version'] != version or identity['source'] != 'local-candidate':
                    raise ValueError('native executable identity mismatch')
            artifacts.append({'path':binary.name, 'target':target, 'sha256':digest(binary), 'size':binary.stat().st_size})
        if inputs() != source:
            raise ValueError('build inputs changed; candidates discarded')
        receipt = {'status':'local-candidate-not-a-release', 'version':version,
                   'commit':'unknown', 'toolchain':run('go', 'env', 'GOVERSION'),
                   'inputs_sha256':fingerprint, 'inputs':source, 'artifacts':artifacts,
                   'notice_files':notices}
        (staging/'manifest.json').write_text(json.dumps(receipt, indent=2)+'\n')
        checksum_files = ([staging/a['path'] for a in artifacts]+[staging/'manifest.json']
                          +[staging/name for name in notices])
        (staging/'SHA256SUMS').write_text(''.join(
            digest(p)+'  '+p.relative_to(staging).as_posix()+'\n' for p in checksum_files))
        staging.rename(output)
    print(json.dumps({'output':str(output), 'version':version, 'targets':list(TARGETS)}))

if __name__ == '__main__':
    main()
