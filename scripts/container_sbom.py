#!/usr/bin/env python3
"""Generate an offline SBOM for one immutable local image ID; never publish it."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import tarfile

SYFT = 'anchore/syft@sha256:678bfa565b60f747aac0f8e964fe5588a24445b8d0a480e91f6efd70020dfbb0'

def sha(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--offline', action='store_true', help='use the cached pinned scanner without contacting a registry')
    parser.add_argument('image_id', help='immutable local image ID: sha256:<64 hex>')
    parser.add_argument('output', type=Path, help='new directory outside the source tree')
    args = parser.parse_args()
    if not re.fullmatch(r'sha256:[0-9a-f]{64}', args.image_id):
        parser.error('use an immutable local image ID, not a mutable tag')
    output = args.output.resolve()
    source = Path(__file__).resolve().parents[1]
    if output.is_relative_to(source):
        parser.error('output must be outside the source tree')
    inspected = json.loads(subprocess.check_output(['docker', 'image', 'inspect', args.image_id]))[0]
    if inspected['Id'] != args.image_id:
        raise ValueError('image identity mismatch')
    if args.offline:
        subprocess.run(['docker', 'image', 'inspect', SYFT], check=True, stdout=subprocess.DEVNULL)
    else:
        subprocess.run(['docker', 'pull', SYFT], check=True)
    output.mkdir(parents=True, exist_ok=False)
    archive = output / 'image.tar'
    subprocess.run(['docker', 'save', args.image_id, '-o', str(archive)], check=True)
    # Modern Docker stores an OCI index as Id; Syft reports the platform config
    # digest. Read the saved archive to bind both identities without conflating them.
    with tarfile.open(archive) as saved:
        manifest = json.load(saved.extractfile('manifest.json'))
        if len(manifest) != 1:
            raise ValueError('expected one platform image in archive')
        config_id = 'sha256:' + hashlib.sha256(
            saved.extractfile(manifest[0]['Config']).read()).hexdigest()
    subprocess.run(['docker', 'run', '--rm', '--network', 'none',
                    '-e', 'SYFT_CHECK_FOR_APP_UPDATE=false',
                    '-v', str(archive)+':/input/image.tar:ro',
                    '-v', str(output)+':/output', SYFT,
                    'scan', 'docker-archive:/input/image.tar',
                    '-o', 'cyclonedx-json=/output/container.cdx.json',
                    '-o', 'syft-json=/output/container.syft.json'], check=True)
    sbom = json.loads((output / 'container.syft.json').read_text())
    if sbom['source']['metadata']['imageID'] != config_id:
        raise ValueError('SBOM image identity mismatch')
    if sbom['descriptor']['version'] != '1.51.0':
        raise ValueError('unexpected SBOM tool version')
    if not any(p['name'] == 'stdlib' for p in sbom['artifacts']):
        raise ValueError('SBOM omitted Go standard library')
    receipt = {'image_id': args.image_id, 'config_digest': config_id, 'scanner': SYFT,
               'platform': inspected['Os']+'/'+inspected['Architecture'],
               'packages': len(sbom['artifacts']),
               'sha256': {name:sha(output/name) for name in
                          ['image.tar', 'container.cdx.json', 'container.syft.json']}}
    (output / 'receipt.json').write_text(json.dumps(receipt, indent=2)+'\n')
    print(json.dumps(receipt, indent=2))

if __name__ == '__main__':
    main()
