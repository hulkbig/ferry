#!/usr/bin/env python3
"""Focused offline source checks, with positive controls for each bundled rule."""
import json
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
IMAGE = 'semgrep/semgrep@sha256:12672acdb0949e19f9f6a4c2b288edd0b404f268f0ca7738a2c06f372f50362e'
RULES = ROOT/'scripts/semgrep_rules.yml'
EXPECTED = {'go-exec-shell-literal', 'go-tls-insecure-skip-verify', 'go-legacy-weak-cipher'}

def scan(directory):
    result = subprocess.run(['docker', 'run', '--rm', '--network', 'none',
        '-e', 'SEMGREP_SEND_METRICS=off', '-e', 'SEMGREP_VERSION_DETECT=off',
        '-v', str(directory)+':/src:ro', '-v', str(RULES)+':/rules.yml:ro',
        IMAGE, 'semgrep', 'scan', '--metrics', 'off', '--disable-version-check',
        '--strict', '--config', '/rules.yml', '--json', '/src'],
        capture_output=True, text=True, timeout=600)
    if result.returncode != 0:
        raise RuntimeError('source scanner failed: '+str(result.returncode))
    report = json.loads(result.stdout)
    if report.get('errors'):
        raise RuntimeError('source scanner reported incomplete analysis')
    if not report.get('paths', {}).get('scanned'):
        raise RuntimeError('source scanner scanned no files')
    expected = {p.relative_to(directory).as_posix() for p in directory.rglob('*.go')}
    actual = {p.removeprefix('/src/') for p in report['paths']['scanned']}
    if expected - actual:
        raise RuntimeError('Go files omitted from scan: '+repr(sorted(expected-actual)))
    return report

def main():
    subprocess.run(['docker', 'pull', IMAGE], check=True, stdout=subprocess.DEVNULL)
    with tempfile.TemporaryDirectory(prefix='ferry-source-control-') as temp:
        directory = Path(temp)
        (directory/'unsafe.go').write_text('''package control
import ("os/exec"; "crypto/tls"; "crypto/des")
func unsafe(command string) {
 exec.Command("sh", "-c", command)
 _ = tls.Config{InsecureSkipVerify: true}
 _, _ = des.NewCipher([]byte("12345678"))
}
''')
        findings = scan(directory)['results']
        ids = {f['check_id'].split('.')[-1] for f in findings}
        if ids != EXPECTED or len(findings) != len(EXPECTED):
            raise RuntimeError('source scanner positive controls failed')
    report = scan(ROOT)
    if report['results']:
        for finding in report['results']:
            print(json.dumps({'rule':finding['check_id'], 'file':finding['path'], 'line':finding['start']['line']}))
        raise RuntimeError('source findings require review')
    print(json.dumps({'focused_source_checks':'pass', 'positive_controls':len(EXPECTED),
                      'files_scanned':len(report['paths']['scanned']), 'network':'none'}))

if __name__ == '__main__':
    main()
