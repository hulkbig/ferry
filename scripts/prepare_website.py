#!/usr/bin/env python3
"""Generate VitePress pages from the repository's bilingual source documentation."""
from pathlib import Path
from html import escape
import re
import shutil

ROOT = Path(__file__).resolve().parents[1]
SITE = ROOT/'website'
PAGES = {
    'docs/README.md':'guide/index.md', 'docs/README.en.md':'en/guide/index.md',
    'CONTRIBUTING.md':'contributing.md', 'CONTRIBUTING.en.md':'en/contributing.md',
    'SECURITY.md':'security.md', 'SECURITY.en.md':'en/security.md',
    'docs/development.md':'development.md', 'docs/development.en.md':'en/development.md',
}
for guide in ('docker','faq','troubleshooting','hermes','autoreset','logical-models','credential-import','codex-usage-protection'):
    PAGES[f'docs/guide/{guide}.md']=f'guide/{guide}.md'
    PAGES[f'docs/guide/{guide}.en.md']=f'en/guide/{guide}.md'

def link(source, url):
    if re.match(r'[a-zA-Z][\w+.-]*:',url) or url.startswith('#'):return url
    path,sep,fragment=url.partition('#')
    target=(ROOT/source).parent/path
    target=target.resolve().relative_to(ROOT).as_posix()
    if target in PAGES:
        dest='/'+PAGES[target].removesuffix('.md')
        if dest.endswith('/index'):dest=dest.removesuffix('index')
    else:dest='https://github.com/hulkbig/ferry/blob/main/'+target
    return dest+('#'+fragment if sep else '')

def prepare(site=SITE):
    site = Path(site)
    content=site/'content';content.mkdir(parents=True, exist_ok=True)
    for source,dest in PAGES.items():
        body=(ROOT/source).read_text()
        # Escape angle-bracket shell placeholders outside code fences for Vue's Markdown parser.
        parts=re.split(r'(```.*?```|`[^`\n]+`)',body,flags=re.S)
        for i in range(0,len(parts),2):parts[i]=re.sub(r'<([a-zA-Z][\w -]*)>',lambda m:escape(m.group(0)),parts[i])
        body=''.join(parts)
        body=re.sub(r'(\]\()([^\s)]+)(\))',lambda m:m[1]+link(source,m[2])+m[3],body)
        p=content/dest;p.parent.mkdir(parents=True,exist_ok=True);p.write_text(body)
    for language,path in [('zh','index.md'),('en','en/index.md')]:
        p=content/path;p.parent.mkdir(parents=True,exist_ok=True)
        p.write_text('---\nlayout: page\nsidebar: false\noutline: false\n---\n\n<HomePage language="'+language+'" />\n')
    public=site/'public';assets=public/'assets';assets.mkdir(parents=True,exist_ok=True)
    for name in ['brand.svg','brand-reverse.svg']:shutil.copyfile(ROOT/'web/console'/name,assets/name)
    for p in assets.glob('flow-*.svg'):
        if not (ROOT/'docs/assets'/p.name).exists():p.unlink()
    for p in (ROOT/'docs/assets').glob('flow-*.svg'):shutil.copyfile(p,assets/p.name)
    for language in ['zh','en']:shutil.copyfile(ROOT/f'docs/assets/gateway-overview-{language}.svg',assets/f'gateway-overview-{language}.svg')
    (public/'licenses.txt').write_text((ROOT/'LICENSE').read_text()+'\n\n'+(ROOT/'NOTICE').read_text()+'\n\nLobe Icons\n'+(ROOT/'LICENSES/LobeIcons/LICENSE').read_text()+'\n\nBundled website JavaScript dependency notices: assets/third-party-licenses.json\n')
    (public/'.nojekyll').write_text('')
    (public/'CNAME').write_text('ferry.bighulk.dev\n')
    print(f'Prepared {len(PAGES)} documentation pages and two landing pages')

if __name__=='__main__':prepare()
