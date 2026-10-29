#!/usr/bin/env python3
"""Offline pinned secret scans with positive controls and exact synthetic fixtures."""
import json
import pathlib
import subprocess
import tempfile
import sys
import argparse
import hashlib

ROOT = pathlib.Path(__file__).resolve().parents[1]
GITLEAKS = 'ghcr.io/gitleaks/gitleaks@sha256:75bdb2b2f4db213cde0b8295f13a88d6b333091bbfbf3012a4e083d00d31caba'
TRUFFLEHOG = 'trufflesecurity/trufflehog@sha256:562bc231afa9de3d04de44cfe624252b08207de1fc3cebc5e7ed92bed7f279e4'
# These exact strings intentionally test rejection of credentials embedded in URLs.
FIXTURES = {
    'internal/credentialimport/parse_test.go': ('user', 'secret', 'example.test'),
    'internal/backuppolicy/policy_test.go': ('key', 'secret', 's3.example.com'),
    'skills/ferry/scripts/test_sync_opencodex_fast.py': ('user', 'secret', 'gateway.example'),
    'internal/nativeauth/xai_meta_test.go': ('user', 'secret', 'auth.x.ai'),
    'web/console/native-oauth.test.mjs': ('u', 'password', 'example.com'),
}

def scan(image, path, args):
    result = subprocess.run(['docker', 'run', '--rm', '--pull', 'never', '--network', 'none', '-v',
                             str(path) + ':/scan:ro', image] + args,
                            capture_output=True, text=True, timeout=600)
    return result

def truffle(path):
    r = scan(TRUFFLEHOG, path, ['filesystem', '/scan', '--no-verification', '--no-update', '--fail', '--json'])
    if r.returncode not in (0, 183):
        raise RuntimeError('TruffleHog failed: exit ' + str(r.returncode))
    return [json.loads(line) for line in r.stdout.splitlines() if line.strip()]

def reviewed_gitleaks(row):
    name = row.get('File', '').removeprefix('/scan/')
    path = ROOT / name
    if not path.resolve().is_relative_to(ROOT) or not path.is_file():
        return False
    start, end = row.get('StartLine', 0), row.get('EndLine', 0)
    if start < 1 or end < start:
        return False
    context = '\n'.join(path.read_text().splitlines()[start-1:end])
    secret_hash = hashlib.sha256(row.get('Secret', '').encode()).hexdigest()
    context_hash = hashlib.sha256(context.encode()).hexdigest()
    exceptions = json.loads((ROOT/'scripts/secret_scan_exceptions.json').read_text())
    return any(item['file'] == name and item['rule'] == row.get('RuleID')
               and item['value_digest'] == secret_hash
               and item['context_sha256'] == context_hash for item in exceptions)

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--offline', action='store_true',
                        help='require locally cached pinned images without contacting registries')
    options = parser.parse_args()
    # Fetch images before mounting source. Actual scans have no network.
    for image in (GITLEAKS, TRUFFLEHOG):
        if not options.offline:
            subprocess.run(['docker', 'pull', image], check=True, stdout=subprocess.DEVNULL)
        metadata = json.loads(subprocess.check_output(['docker', 'image', 'inspect', image]))
        if len(metadata) != 1 or image not in metadata[0].get('RepoDigests', []):
            raise RuntimeError('cached scanner does not match pinned repository digest')
    with tempfile.TemporaryDirectory(prefix='ferry-scan-control-') as temp:
        control = pathlib.Path(temp)
        (control / 'synthetic.txt').write_text('token = "ghp_' + 'AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AB' + '"\nhttps://user:secret@control.example\n')
        positive = scan(GITLEAKS, control, ['detect', '--no-git', '--source', '/scan', '--redact', '--exit-code', '7'])
        if positive.returncode != 7:
            raise RuntimeError('Gitleaks positive control did not fail as expected')
        if not truffle(control):
            raise RuntimeError('TruffleHog positive control did not detect synthetic credentials')
    # Keep raw findings only in captured process memory, never logs or reports.
    leaks = scan(GITLEAKS, ROOT, ['detect', '--no-git', '--source', '/scan',
                                '--report-format', 'json', '--report-path', '/dev/stdout',
                                '--exit-code', '7'])
    if leaks.returncode not in (0, 7):
        raise RuntimeError('Gitleaks candidate scan failed: exit ' + str(leaks.returncode))
    leak_rows = json.loads(leaks.stdout)
    if any(not reviewed_gitleaks(row) for row in leak_rows):
        raise RuntimeError('Gitleaks candidate has unreviewed findings')
    findings = truffle(ROOT)
    unexpected = []
    for row in findings:
        file = row.get('SourceMetadata', {}).get('Data', {}).get('Filesystem', {}).get('file', '')
        name = file.removeprefix('/scan/')
        fixture = FIXTURES.get(name)
        expected = ('https://' + fixture[0] + ':' + fixture[1] + '@' + fixture[2]) if fixture else None
        if row.get('DetectorName') != 'URI' or not expected or row.get('Raw') != expected:
            unexpected.append({'file': name, 'detector': row.get('DetectorName')})
    if unexpected:
        # Never print matched values, scanner output or credentials.
        print(json.dumps({'unexpected': unexpected}), file=sys.stderr)
        return 1
    print(json.dumps({'gitleaks': 'pass', 'trufflehog': 'pass', 'positive_controls': 'pass',
                      'reviewed_gitleaks_findings': len(leak_rows),
                      'reviewed_synthetic_uri_findings': len(findings), 'network': 'none',
                      'image_acquisition': 'cached-digest' if options.offline else 'registry-digest'}))
    return 0

if __name__ == '__main__':
    try:
        sys.exit(main())
    except (RuntimeError, subprocess.SubprocessError, ValueError) as exc:
        print(type(exc).__name__ + ': scan incomplete', file=sys.stderr)
        sys.exit(1)
