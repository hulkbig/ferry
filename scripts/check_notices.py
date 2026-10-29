#!/usr/bin/env python3
"""Check notice bytes and runtime module coverage for supported build targets.

Coverage checks prevent drift; they do not decide legal provenance or completeness
of nested licenses, container assets or first-party licensing.
"""
import hashlib
import json
import os
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[1]
TARGETS = ('darwin/arm64', 'linux/amd64', 'linux/arm64')

def command(*args, env=None):
    return subprocess.check_output(args, cwd=ROOT, env=env, text=True).strip()

def check_bytes(rows):
    for row in rows:
        path = ROOT / row['path']
        if not path.resolve().is_relative_to((ROOT / 'LICENSES').resolve()):
            raise ValueError('notice path escapes LICENSES')
        if hashlib.sha256(path.read_bytes()).hexdigest() != row['sha256']:
            raise ValueError('notice checksum mismatch: ' + row['path'])

def main():
    inventory = json.loads((ROOT / 'docs/runtime-dependencies.json').read_text())
    declared = {(row['module'], row['version']) for row in inventory}
    if len(declared) != len(inventory):
        raise ValueError('duplicate runtime module')
    for row in inventory:
        if not row['notices']:
            raise ValueError('module has no notice: ' + row['module'])
        check_bytes(row['notices'])
    actual = set()
    recorded = {n['path']:n['sha256'] for row in inventory for n in row['notices']}
    for target in TARGETS:
        system, arch = target.split('/')
        env = dict(os.environ, GOOS=system, GOARCH=arch, CGO_ENABLED='0')
        output = command('go', 'list', '-deps', '-f',
                         '{{if .Module}}{{if not .Module.Main}}{{.Module.Path}}\t{{.Module.Version}}\t{{.Module.Dir}}\t{{.Dir}}{{end}}{{end}}',
                         './cmd/ferry', env=env)
        for line in output.splitlines():
            if not line.strip():
                continue
            module, version, module_dir, package_dir = line.split('\t')
            actual.add((module, version))
            base, directory = Path(module_dir), Path(package_dir)
            if not directory.is_relative_to(base):
                raise ValueError('package directory outside module')
            while True:
                for notice in directory.iterdir():
                    if notice.is_file() and notice.name.upper().startswith(('LICENSE', 'COPYING', 'NOTICE', 'PATENTS')):
                        path = ('LICENSES/dependencies/'+module.replace('/', '__')+'@'+version
                                +'/'+notice.relative_to(base).as_posix())
                        if recorded.get(path) != hashlib.sha256(notice.read_bytes()).hexdigest():
                            raise ValueError('missing or changed source notice: '+path)
                if directory == base:
                    break
                directory = directory.parent
    if actual != declared:
        raise ValueError('runtime inventory drift: missing=' + repr(sorted(actual-declared))
                         + ' stale=' + repr(sorted(declared-actual)))
    toolchain = json.loads((ROOT / 'docs/toolchain-notices.json').read_text())
    if command('go', 'env', 'GOVERSION') != toolchain['version']:
        raise ValueError('toolchain version changed; review notices')
    check_bytes(toolchain['notices'])
    goroot = Path(command('go', 'env', 'GOROOT'))
    for row in toolchain['notices']:
        if (goroot / row['source']).read_bytes() != (ROOT / row['path']).read_bytes():
            raise ValueError('Go notice differs from active toolchain')
    print('PASS runtime module coverage for ' + ', '.join(TARGETS)
          + '; dependency and Go notice checksums')

if __name__ == '__main__':
    main()
