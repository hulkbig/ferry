#!/usr/bin/env python3
"""Export a standalone VitePress source package; never initialize Git or publish."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import tempfile

from prepare_website import PAGES, ROOT, prepare


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def export(output):
    output = Path(output).resolve()
    if output == ROOT or output.is_relative_to(ROOT):
        raise ValueError('choose a new directory outside the product source tree')
    if output.exists():
        raise ValueError('output already exists; export to a new directory to preserve local changes')
    output.parent.mkdir(parents=True, exist_ok=True)
    source_files = set(PAGES)
    source_files.update(('scripts/prepare_website.py', 'scripts/export_docs.py',
                         'LICENSE', 'NOTICE', 'web/console/brand.svg',
                         'web/console/brand-reverse.svg'))
    with tempfile.TemporaryDirectory(prefix='.ferry-doc-export-', dir=output.parent) as temporary:
        staging = Path(temporary) / 'source'
        staging.mkdir()
        prepare(staging)
        for name in ('package.json', 'package-lock.json'):
            shutil.copy2(ROOT/'website'/name, staging/name)
            source_files.add('website/'+name)
        for path in (ROOT/'website/.vitepress').rglob('*'):
            relative = path.relative_to(ROOT/'website')
            if any(part in ('cache', 'dist') for part in relative.parts) or not path.is_file():
                continue
            destination = staging/relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(path, destination)
            source_files.add(path.relative_to(ROOT).as_posix())
        for name in ('LICENSE', 'NOTICE'):
            shutil.copy2(ROOT/name, staging/name)
        shutil.copytree(ROOT/'LICENSES', staging/'LICENSES')
        source_files.update(path.relative_to(ROOT).as_posix()
                            for path in (ROOT/'LICENSES').rglob('*') if path.is_file())
        source_files.update(path.relative_to(ROOT).as_posix()
                            for path in (ROOT/'docs/assets').glob('flow-*.svg'))
        source_files.update(f'docs/assets/gateway-overview-{language}.svg' for language in ('zh', 'en'))
        (staging/'.gitignore').write_text('node_modules/\n.vitepress/cache/\n.vitepress/dist/\n.DS_Store\n')
        (staging/'README.md').write_text('''# Ferry documentation source

This is a standalone VitePress source package exported from the Ferry public
product sources. It contains the bilingual website, documentation pages,
animation assets, dependency lockfile and notices. It needs no application
backend or neighboring product checkout.

Use Node.js 22.12 or a newer compatible release:

```sh
npm ci
FERRY_SITE_BASE=/ npm run build
npm run preview
```

The default build output is `.vitepress/dist/`. Set `FERRY_SITE_BASE=/` when
hosting at a domain root, or use the actual subdirectory including both slashes.
Development preview uses `npm run dev`. Nothing here deploys automatically.

`content/`, `public/` and `.vitepress/theme/` are source files in this package;
keep them when copying it. Do not distribute `node_modules/` or build caches.
The build includes dependency notices at `assets/third-party-licenses.json`,
alongside the product and artwork notices at `licenses.txt`.

The canonical text and theme are maintained in Ferry's product repository.
To refresh this package, run `python3 scripts/export_docs.py <new-directory>`
there, review the exported changes, and keep local edits before replacing an
older export. `source-manifest.json` records source and output hashes without
including a private repository identity or history.

Production uses https://ferry.bighulk.dev/ with deployment base `/`. The generated
CNAME is for that domain; configure Pages and DNS separately before publishing.

Building this source package does not deploy a website or create a release.
Installation files and published images are available through Ferry releases.
''')
        manifest = {
            'format': 1,
            'source': 'Ferry public product documentation',
            'published': False,
            'inputs': {name: digest(ROOT/name) for name in sorted(source_files)},
            'files': {path.relative_to(staging).as_posix(): digest(path)
                      for path in sorted(staging.rglob('*')) if path.is_file()},
        }
        (staging/'source-manifest.json').write_text(json.dumps(manifest, indent=2)+'\n')
        staging.rename(output)
    print(json.dumps({'output': str(output), 'pages': len(PAGES)+2,
                      'source_files': len(manifest['files'])+1, 'published': False}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('output', type=Path, help='new directory outside the product checkout')
    args = parser.parse_args()
    try:
        export(args.output)
    except ValueError as error:
        parser.error(str(error))


if __name__ == '__main__':
    main()
