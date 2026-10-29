#!/usr/bin/env python3
"""Build the bilingual VitePress site with an explicit deployment base."""
import argparse
import os
from pathlib import Path
import subprocess
from prepare_website import prepare

ROOT = Path(__file__).resolve().parents[1]

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('output',type=Path)
    parser.add_argument('--base',default='./',help='Relative preview base or a deployment path such as /ferry/')
    args=parser.parse_args();output=args.output.resolve()
    if output==ROOT or output.is_relative_to(ROOT):parser.error('choose an output directory outside the source checkout')
    if not (ROOT/'website/node_modules/vitepress/package.json').is_file():parser.error('run npm ci --prefix website first')
    prepare()
    subprocess.run(['npm','run','build','--','--outDir',str(output)],cwd=ROOT/'website',env={**os.environ,'FERRY_SITE_BASE':args.base},check=True)
    print('VitePress website ready: '+str(output))

if __name__=='__main__':main()
