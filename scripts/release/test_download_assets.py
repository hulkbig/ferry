import copy
import hashlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch
import urllib.error
import urllib.request

from download_assets import AssetRedirect, FILES, fetch, validate_release, verify


class BuildAssetsTest(unittest.TestCase):
    sha = 'a' * 40
    tag = 'ci-' + sha + '-123-2'

    def release(self):
        return {'tag_name': self.tag, 'target_commitish': self.sha, 'draft': False, 'prerelease': True, 'immutable': True,
                'assets': [{'name': n, 'id': i + 1, 'state': 'uploaded'} for i, n in enumerate(sorted(FILES))]}

    def test_release_requires_complete_exact_assets_and_source(self):
        good = self.release()
        self.assertEqual(set(validate_release(good, self.tag, self.sha)), FILES)
        for field, value in [('target_commitish', 'main'), ('draft', True), ('prerelease', False), ('immutable', False)]:
            bad = copy.deepcopy(good)
            bad[field] = value
            with self.assertRaises(ValueError):
                validate_release(bad, self.tag, self.sha)
        for assets in [good['assets'][:-1], good['assets'] + [good['assets'][0]],
                       [dict(a, state='new') for a in good['assets']]]:
            with self.assertRaises(ValueError):
                validate_release(dict(good, assets=assets), self.tag, self.sha)

    def test_manifest_rejects_other_run_attempt_and_changed_payload(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for name in FILES - {'release.json'}:
                (root / name).write_bytes(b'checked bytes')
            m = {'schema': 'ferry/release/v1', 'repository': 'hulkbig/ferry', 'repository_id': '12',
                 'commit': self.sha, 'workflow_run_id': '123', 'workflow_run_attempt': '2',
                 'image_digest': 'sha256:' + 'b' * 64,
                 'files': {n: hashlib.sha256((root / n).read_bytes()).hexdigest() for n in FILES - {'release.json'}}}
            (root / 'release.json').write_text(json.dumps(m))
            self.assertEqual(verify(root, 'hulkbig/ferry', '12', self.sha, '123', '2'), m)
            with self.assertRaises(ValueError):
                verify(root, 'hulkbig/ferry', '12', self.sha, '123', '1')
            (root / 'payload.tar').write_bytes(b'changed')
            with self.assertRaises(ValueError):
                verify(root, 'hulkbig/ferry', '12', self.sha, '123', '2')

    def test_manifest_rejects_extra_files_and_paths(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            m = {'schema': 'ferry/release/v1', 'repository': 'hulkbig/ferry', 'repository_id': '12',
                 'commit': self.sha, 'workflow_run_id': '123', 'workflow_run_attempt': '2',
                 'image_digest': 'sha256:' + 'b' * 64, 'files': {'../outside': 'b' * 64}}
            (root / 'release.json').write_text(json.dumps(m))
            with self.assertRaises(ValueError):
                verify(root, 'hulkbig/ferry', '12', self.sha, '123', '2')

    def test_signed_download_does_not_forward_github_token(self):
        request = urllib.request.Request('https://api.github.com/repos/hulkbig/ferry/releases/assets/1',
                                         headers={'Authorization': 'Bearer private-test-token'})
        redirect = AssetRedirect().redirect_request(request, None, 302, '', {},
                                                     'https://release-assets.githubusercontent.com/asset?signature=test')
        self.assertIsNone(redirect.get_header('Authorization'))
        for url in ['http://release-assets.githubusercontent.com/asset', 'https://example.com/asset',
                    'https://user@release-assets.githubusercontent.com/asset',
                    'https://release-assets.githubusercontent.com:444/asset']:
            with self.assertRaises(ValueError):
                AssetRedirect().redirect_request(request, None, 302, '', {}, url)

    @patch('download_assets.time.sleep')
    def test_transient_metadata_failures_retry(self, sleep):
        opener = Mock()
        opener.open.side_effect = [urllib.error.URLError('transient'), TimeoutError(), io.BytesIO(b'{}')]
        self.assertEqual(fetch(opener, urllib.request.Request('https://api.github.com/test')), b'{}')
        self.assertEqual(opener.open.call_count, 3)
        self.assertEqual(sleep.call_count, 2)

    @patch('download_assets.time.sleep')
    def test_partial_asset_is_replaced_by_complete_retry(self, sleep):
        class Interrupted(io.BytesIO):
            headers = {'Content-Length': '6'}
            def read(self, size=-1):
                if self.tell():
                    raise TimeoutError()
                return super().read(3)
        complete = io.BytesIO(b'abcdef')
        complete.headers = {'Content-Length': '6'}
        opener = Mock()
        opener.open.side_effect = [Interrupted(b'abc'), complete]
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / 'payload.tar'
            fetch(opener, urllib.request.Request('https://api.github.com/test'), destination)
            self.assertEqual(destination.read_bytes(), b'abcdef')
            self.assertEqual({p.name for p in destination.parent.iterdir()}, {'payload.tar'})

    @patch('download_assets.time.sleep')
    def test_access_and_missing_asset_errors_do_not_retry(self, sleep):
        for status in (401, 403, 404):
            opener = Mock()
            opener.open.side_effect = urllib.error.HTTPError('https://api.github.com/test', status, '', {}, None)
            with self.assertRaises(urllib.error.HTTPError):
                fetch(opener, urllib.request.Request('https://api.github.com/test'))
            self.assertEqual(opener.open.call_count, 1)
        sleep.assert_not_called()

    @patch('download_assets.time.sleep')
    def test_exhausted_transport_does_not_report_signed_url(self, sleep):
        opener = Mock()
        opener.open.side_effect = urllib.error.URLError('signed-url-secret')
        with self.assertRaisesRegex(RuntimeError, '^release download transport retries exhausted$'):
            fetch(opener, urllib.request.Request('https://api.github.com/test'), attempts=2)
        self.assertEqual(opener.open.call_count, 2)


if __name__ == '__main__':
    unittest.main()
