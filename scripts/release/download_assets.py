#!/usr/bin/env python3
"""Download one successful main check's build assets, with no build authority."""
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import ssl
import time
import urllib.parse
import urllib.error
import urllib.request

FILES = {'release.json', 'payload.tar', 'ferry-receiver', 'verify_deployment.sh'}


def fetch(opener, request, destination=None, attempts=5):
    """Retry transport failures; publish only a complete temporary download."""
    for attempt in range(attempts):
        partial = destination.with_name(destination.name + '.part') if destination else None
        created = False
        try:
            with opener.open(request, timeout=30) as response:
                if destination is None:
                    body = response.read(1024 * 1024 + 1)
                    if len(body) > 1024 * 1024:
                        raise ValueError('release metadata too large')
                    return body
                with partial.open('xb') as output:
                    created = True
                    size = 0
                    while chunk := response.read(1024 * 1024):
                        output.write(chunk)
                        size += len(chunk)
                    length = response.headers.get('Content-Length')
                    if length is not None and size != int(length):
                        raise http.client.IncompleteRead(b'', int(length) - size)
                    output.flush()
                    os.fsync(output.fileno())
                if destination.exists():
                    raise ValueError('release destination already exists')
                partial.rename(destination)
                created = False
                return None
        except (urllib.error.URLError, TimeoutError, ConnectionError, ssl.SSLError,
                http.client.IncompleteRead, http.client.RemoteDisconnected) as error:
            if isinstance(error, urllib.error.HTTPError) and error.code not in {408, 429, 500, 502, 503, 504}:
                raise
            if attempt + 1 == attempts:
                raise RuntimeError('release download transport retries exhausted') from None
            print(json.dumps({'transport_retry': attempt + 1,
                              'stage': destination.name if destination else 'release-metadata'}), flush=True)
            time.sleep(2 ** attempt)
        finally:
            if created:
                partial.unlink()


class AssetRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        target = urllib.parse.urlsplit(newurl)
        if target.scheme != 'https' or target.hostname not in {
            'api.github.com', 'release-assets.githubusercontent.com', 'objects.githubusercontent.com',
        } or target.username or target.password or target.port not in (None, 443):
            raise ValueError('release asset redirect refused')
        redirected = super().redirect_request(req, fp, code, msg, headers, newurl)
        if target.hostname != urllib.parse.urlsplit(req.full_url).hostname:
            redirected.remove_header('Authorization')
        return redirected


def validate_release(release, tag, sha):
    if release.get('tag_name') != tag or release.get('target_commitish') != sha or release.get('draft') or not release.get('prerelease') or release.get('immutable') is not True:
        raise ValueError('build release identity mismatch')
    assets = release['assets']
    if len(assets) != len(FILES) or {a['name'] for a in assets} != FILES:
        raise ValueError('unexpected or missing release assets')
    if any(type(a['id']) is not int or a['id'] <= 0 or a.get('state') != 'uploaded' for a in assets):
        raise ValueError('release assets not ready')
    return {a['name']: a['id'] for a in assets}


def verify(root, repo, repo_id, sha, run_id, attempt):
    m = json.loads((root / 'release.json').read_text())
    expected = {'schema': 'ferry/release/v1', 'repository': repo, 'repository_id': repo_id,
                'commit': sha, 'workflow_run_id': run_id, 'workflow_run_attempt': attempt}
    if any(m.get(k) != v for k, v in expected.items()):
        raise ValueError('checked build identity mismatch')
    if not re.fullmatch(r'sha256:[0-9a-f]{64}', m['image_digest']):
        raise ValueError('invalid image digest')
    if set(m['files']) != FILES - {'release.json'} or {p.name for p in root.iterdir()} != FILES:
        raise ValueError('unexpected build file set')
    for name, digest in m['files'].items():
        if not re.fullmatch('[0-9a-f]{64}', digest):
            raise ValueError('invalid file digest')
        path = root / name
        if not path.is_file() or path.is_symlink():
            raise ValueError('invalid release file')
        with path.open('rb') as f:
            if hashlib.file_digest(f, 'sha256').hexdigest() != digest:
                raise ValueError('release file digest mismatch')
    return m


def main():
    repo = os.environ['GITHUB_REPOSITORY']
    sha = os.environ['RELEASE_SHA']
    run_id, attempt = os.environ['RELEASE_RUN_ID'], os.environ['RELEASE_RUN_ATTEMPT']
    if not re.fullmatch('[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repo) or not re.fullmatch('[0-9a-f]{40}', sha):
        raise ValueError('invalid repository or commit')
    if any(not re.fullmatch('[1-9][0-9]*', value) for value in (run_id, attempt)):
        raise ValueError('invalid check run identity')
    tag = f'ci-{sha}-{run_id}-{attempt}'
    root = Path(os.environ['RUNNER_TEMP']) / 'ferry-release'
    root.mkdir(mode=0o700)
    opener = urllib.request.build_opener(AssetRedirect)

    def request(path, accept):
        return urllib.request.Request('https://api.github.com/repos/' + repo + path,
                                      headers={'Authorization': 'Bearer ' + os.environ['GH_TOKEN'],
                                               'Accept': accept, 'X-GitHub-Api-Version': '2022-11-28'})

    release = json.loads(fetch(opener, request('/releases/tags/' + tag, 'application/vnd.github+json')))
    assets = validate_release(release, tag, sha)
    for name, asset_id in sorted(assets.items()):
        fetch(opener, request('/releases/assets/' + str(asset_id), 'application/octet-stream'), root / name)
    verify(root, repo, os.environ['REPOSITORY_ID'], sha, run_id, attempt)
    print(json.dumps({'commit': sha, 'workflow_run_id': run_id, 'attempt': attempt, 'verified_files': len(FILES)}))


if __name__ == '__main__':
    main()
