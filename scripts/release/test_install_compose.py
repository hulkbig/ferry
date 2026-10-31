"""Protect image-only install paths and the legacy persistent data layout."""
import json
import os
from pathlib import Path
import subprocess
import unittest

ROOT = Path(__file__).resolve().parents[2]

class InstallComposeTest(unittest.TestCase):
    def config(self, *files):
        env = {k:v for k,v in os.environ.items() if k not in ('FERRY_IMAGE','FERRY_DATA_VOLUME')}
        args = ['docker','compose']
        for name in files: args += ['-f',str(ROOT/name)]
        return json.loads(subprocess.check_output(args+['config','--format','json'],env=env))['services']['ferry']

    def test_default_installers_only_pull_the_same_published_image(self):
        regular=self.config('compose.yaml');legacy=self.config('deploy/compose.yaml')
        for service in (regular,legacy):
            self.assertNotIn('build',service)
            self.assertRegex(service['image'],r'^bighulk/ferry:\d+\.\d+\.\d+$')
        self.assertEqual(regular['image'],legacy['image'])
        self.assertIsNone(regular.get('command'))
        self.assertEqual(legacy['command'],['-data-dir','/data','-config','/etc/ferry/config.yaml'])

    def test_headless_uses_existing_image_and_new_install_volume_layout(self):
        regular=self.config('compose.yaml');headless=self.config('deploy/compose.headless.yaml')
        self.assertNotIn('build',headless)
        self.assertEqual(headless['image'],regular['image'])
        self.assertEqual(headless['command'],['-data-dir','/data/state','-config','/etc/ferry/config.yaml','-headless'])
        self.assertEqual([p['target'] for p in headless['ports']],[7890])
        self.assertEqual(headless['volumes'],regular['volumes'])

    def test_source_build_requires_explicit_developer_override(self):
        developer=self.config('deploy/compose.yaml','deploy/compose.build.yaml')
        self.assertIn('build',developer)
        self.assertEqual(developer['image'],'ferry:local')

if __name__=='__main__': unittest.main()
