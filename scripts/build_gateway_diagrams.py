#!/usr/bin/env python3
"""Build six self-contained SVG animations shared by the README and VitePress."""
from pathlib import Path
import re
from html import escape

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / 'docs/assets'
BLUE, GREEN, RED, AMBER = '#79d8ff', '#a3edbc', '#ff929c', '#f4c575'

def icon(name, x, y, size=30, prefix='icon'):
    if name == 'hermes':
        return f'<rect x="{x}" y="{y}" width="{size}" height="{size}" rx="7" fill="#c79d63"/><text x="{x+size/2}" y="{y+size*.74}" text-anchor="middle" font-size="{size*.68}" fill="#171b20">H</text>'
    path = ROOT/'web/console/brand-reverse.svg' if name == 'ferry' else OUT/'vendor/lobe-icons'/f'{name}.svg'
    svg = path.read_text()
    view = re.search(r'viewBox="([^"]+)"', svg).group(1)
    body = svg[svg.index('>')+1:svg.rindex('</svg>')]
    body = re.sub(r'<title.*?</title>', '', body, flags=re.S)
    for old in re.findall(r'id="([^"]+)"', body):
        body = body.replace(f'id="{old}"', f'id="{prefix}-{old}"').replace(f'#{old}', f'#{prefix}-{old}')
    return f'<svg x="{x}" y="{y}" width="{size}" height="{size}" viewBox="{view}" fill="currentColor" color="#e8eef5">{body}</svg>'

def text(x, y, value, size=20, color='#e9eef4', extra=''):
    return f'<text x="{x}" y="{y}" font-size="{size}" fill="{color}" {extra}>{escape(value)}</text>'

def node(x, y, label, mark, sub='', width=226, height=72, prefix='node'):
    baseline=y+31 if sub else y+height/2+7
    return (f'<rect x="{x}" y="{y}" width="{width}" height="{height}" rx="13" fill="#171e26" stroke="#34404b"/>'
            +icon(mark,x+17,y+18,30,prefix)+text(x+60,baseline,label,20)
            +(text(x+60,y+56,sub,14,'#93a5b5') if sub else ''))

def core():
    return ('<rect x="386" y="172" width="188" height="152" rx="21" fill="#242424" stroke="#719c8b"/>'
            +icon('ferry',452,193,56)+text(480,295,'Ferry',29,extra='text-anchor="middle" font-weight="650"'))

def wire(d):
    return f'<path d="{d}" fill="none" stroke="#425262" stroke-width="2"/>'

def visible(start, end):
    return f'<animate attributeName="opacity" values="0;0;1;1;0;0" keyTimes="0;{start};{start+.001};{end};{min(end+.001,.999)};1" dur="8s" repeatCount="indefinite"/>'

def packet(d, start, end, color=BLUE, reverse=False):
    points='1;1;0;0' if reverse else '0;0;1;1'
    return (f'<g class="motion" opacity="0">{visible(start,end)}<circle r="8" fill="{color}" opacity=".2">'
            f'<animateMotion path="{d}" keyPoints="{points}" keyTimes="0;{start};{end};1" calcMode="linear" dur="8s" repeatCount="indefinite"/></circle>'
            f'<circle r="4.5" fill="{color}" stroke="#eefbff" stroke-width=".8"><animateMotion path="{d}" keyPoints="{points}" keyTimes="0;{start};{end};1" calcMode="linear" dur="8s" repeatCount="indefinite"/></circle></g>')

def state(x,y,label,start,end,color):
    return f'<g class="motion" opacity="0">{visible(start,end)}'+text(x,y,label,16,color)+'</g>'

def draw(scene, language):
    en=language=='en';prefix=f'ferry-{scene}-{language}'
    titles={'request':('请求发出','Send a request'),'response':('结果返回','Return the result'),'fallback':('自动接力','Automatic fallback'),'queue':('排队限流','Queue through rate limits'),'logical':('逻辑模型','Logical models'),'autoreset':('AutoReset','AutoReset')}
    title=titles[scene][en];num=['request','response','fallback','queue','logical','autoreset'].index(scene)+1
    desc=('Illustrative routing, not live traffic. Compatible routes and recovery budgets apply.' if en else '流程示意，不是真实流量；备用线路须兼容，等待与恢复受预算约束。')
    head=f'''<svg xmlns="http://www.w3.org/2000/svg" width="960" height="480" viewBox="0 0 960 480" role="img" aria-labelledby="{prefix}-title {prefix}-desc" id="{prefix}">
<title id="{prefix}-title">{escape(title)}</title><desc id="{prefix}-desc">{escape(desc)}</desc>
<!-- Original Ferry animation. Product marks: Lobe Icons 1.100.0, MIT. Hermes uses a generic H monogram, not its official logo. -->
<style>#{prefix}{{color:#e9eef4;font-family:-apple-system,BlinkMacSystemFont,Segoe UI,Arial,PingFang SC,Microsoft YaHei,sans-serif}}@media(prefers-reduced-motion:reduce){{#{prefix}:not(.motion-enabled) .motion{{display:none}}}}</style>
<rect x="1" y="1" width="958" height="478" rx="20" fill="#10151c" stroke="#303c47"/>
<path d="M32 68H928" stroke="#28343f"/>
'''+text(34,43,f'0{num} / '+title,20)+text(925,42,'FERRY',12,'#778a9d','text-anchor="end" letter-spacing="3"')
    body=[];moves=[];C='M260 246 C310 246 332 246 386 246';R='M574 246 C623 246 654 246 700 246'
    if scene=='request':
        for i,(name,mark,up,umark) in enumerate([('Claude Code','claudecode-color','Anthropic','claude-color'),('Hermes Agent','hermes','OpenAI','openai'),('Codex','codex-color','DeepSeek','deepseek-color')]):
            y=106+i*104;cy=y+36;ey=210+i*34
            left=f'M260 {cy} C315 {cy} 325 {ey} 386 {ey}';right=f'M574 {ey} C630 {ey} 643 {cy} 700 {cy}'
            body += [wire(left),wire(right),node(34,y,name,mark,prefix=prefix+str(i)),node(700,y,up,umark,prefix=prefix+'u'+str(i))]
            start=.03+i*.29;moves += [packet(left,start,start+.10),packet(right,start+.13,start+.25)]
        footer=('Choose an eligible model and route for each request.' if en else '客户端发出请求，Ferry 选择符合要求的模型与线路。')
    elif scene=='response':
        body += [wire(C),wire(R),node(34,210,'Hermes Agent','hermes',prefix=prefix)]
        body += ['<rect x="700" y="108" width="226" height="254" rx="14" fill="#171e26" stroke="#34404b"/>',icon('openai',717,128,prefix=prefix),text(760,151,'OpenAI',21),text(718,190,'STREAM',12,'#94b8a3','letter-spacing="2"')]
        for i,(zh,eng) in enumerate([('文本片段','Text delta'),('工具参数','Tool arguments'),('完成事件','Completion')]):
            y=213+i*46;body += [f'<rect x="717" y="{y-18}" width="192" height="34" rx="6" fill="#202d2b"/>',text(731,y+4,eng if en else zh,16,'#a3cbb5')]
            body += [f'<rect x="50" y="{312+i*23}" width="{150-i*30}" height="8" rx="4" fill="#30433b"/>']
            start=.04+i*.29;moves += [packet(R,start,start+.10,GREEN,True),packet(C,start+.13,start+.25,GREEN,True)]
        footer=('Streamed text and tool results return to the same client.' if en else '文本、工具参数与完成事件，经同一入口返回客户端。')
    elif scene=='fallback':
        body += [wire(C),node(34,210,'Codex','codex-color',prefix=prefix)]
        paths=[]
        for i,(label,mark,sub) in enumerate([('ChatGPT · A','openai','Primary' if en else '首选线路'),('ChatGPT · B','openai','Fallback' if en else '备用账号'),('OpenAI API','openai','Next route' if en else '后续备用')]):
            y=106+i*104;cy=y+36;ey=210+i*34;d=f'M574 {ey} C630 {ey} 643 {cy} 700 {cy}';paths.append(d)
            body += [wire(d),node(700,y,label,mark,sub,prefix=prefix+str(i))]
        moves += [packet(C,.02,.12),packet(paths[0],.14,.26),packet(paths[1],.45,.6),packet(paths[1],.68,.8,GREEN,True),packet(C,.82,.94,GREEN,True)]
        moves += [f'<g class="motion" opacity="0">{visible(.28,.96)}<rect x="700" y="106" width="226" height="72" rx="13" fill="none" stroke="{RED}" stroke-width="2"/>'+text(907,161,'503',16,RED,'text-anchor="end"')+'</g>',state(902,265,'OK',.62,.96,GREEN)]
        footer=('Primary fails; a compatible fallback completes the request.' if en else '首选线路临时失败，兼容的备用线路接力完成请求。')
    elif scene=='queue':
        C='M260 154 C317 154 330 217 386 217';R='M574 219 C630 219 645 169 700 169'
        body += [wire(C),wire(R),wire('M574 277 C630 277 645 302 700 302'),node(34,118,'Hermes Agent','hermes',prefix=prefix),node(700,126,'ChatGPT','openai','Concurrency 2' if en else '并发上限 2',height=86,prefix=prefix+'a'),node(700,260,'Claude Code','claudecode-color','Another route' if en else '其他线路',height=86,prefix=prefix+'b')]
        for i in range(3):
            body += [f'<rect x="{60+i*58}" y="241" width="44" height="38" rx="7" fill="#2d2a24" stroke="#806b43"/>',text(82+i*58,266,str(i+1),18,AMBER,'text-anchor="middle"')]
            x=395+i*58
            body += [f'<rect x="{x}" y="351" width="46" height="34" rx="7" fill="#282e33" stroke="#566571"/>',text(x+23,374,f'0{i+1}',16,'#6f8596','text-anchor="middle"')]
            moves += [packet(C,.02+i*.06,.15+i*.06,AMBER),f'<g class="motion" opacity="0">{visible(.18+i*.04,.57+i*.12)}<rect x="{x}" y="351" width="46" height="34" rx="7" fill="#4a3c25" stroke="{AMBER}"/>'+text(x+23,374,f'0{i+1}',16,AMBER,'text-anchor="middle"')+'</g>',packet(R,.59+i*.12,.68+i*.12)]
        moves += [state(815,188,'429 · WAIT',.25,.56,AMBER),state(815,188,'1 / 2 · GO',.59,.96,GREEN)]
        body += [text(34,313,'Pending tasks' if en else '等待中的任务',16,'#91a3b4'),text(480,414,'QUEUE',12,'#91a3b4','text-anchor="middle" letter-spacing="3"')]
        footer=('Requests wait during a rate limit, then leave the queue in order.' if en else '遇到限流先等待，容量释放后按序发出请求。')
    elif scene=='logical':
        body += [wire(C),node(34,210,'Hermes Agent','hermes','Client example' if en else '接入案例',prefix=prefix)]
        routes=[]
        for i,(label,mark,sub) in enumerate([('OpenAI','openai','Model A' if en else '模型 A'),('Anthropic','claude-color','Model B' if en else '模型 B'),('DeepSeek','deepseek-color','Model C' if en else '模型 C')]):
            y=106+i*104;cy=y+36;ey=210+i*34;d=f'M574 {ey} C630 {ey} 643 {cy} 700 {cy}';routes.append(d)
            body += [wire(d),node(700,y,label,mark,sub,prefix=prefix+str(i))]
        body += [text(34,326,'model: work',23,GREEN),text(34,357,'Same client configuration' if en else '客户端配置保持不变',15,'#93a5b5')]
        for i,(start,end) in enumerate([(.02,.43),(.53,.94)]):
            moves += [packet(C,start,start+.08),packet(routes[i],start+.10,start+.19),packet(routes[i],start+.23,start+.31,GREEN,True),packet(C,start+.33,end,GREEN,True)]
            moves += [f'<g class="motion" opacity="0">{visible(start,end)}<rect x="700" y="{106+i*104}" width="226" height="72" rx="13" fill="none" stroke="{GREEN}" stroke-width="2"/></g>']
            moves += [state(395,367,'work → '+('A' if i==0 else 'B'),start,end,GREEN)]
        footer=('Change the upstream model between requests. Keep the business endpoint.' if en else '两次请求使用不同上游模型，业务仍调用同一个逻辑模型。')
    elif scene=='autoreset':
        left='M260 246 C314 246 330 246 386 246';right='M574 246 C628 246 648 246 700 246'
        body += [wire(left),wire(right),'<rect x="34" y="170" width="226" height="152" rx="14" fill="#28251f" stroke="#806b43"/>',text(54,205,'RESET CREDIT',13,AMBER,'letter-spacing="1"'),text(54,243,'Available' if en else '可用重置券',23),text(54,280,'Expires in 10 min' if en else '10 分钟后到期',17,AMBER)]
        body += [node(700,170,'Codex','codex-color','Upstream account' if en else '上游账号',height=90,prefix=prefix),text(700,306,'Use 1 credit' if en else '兑换 1 张重置券',18),text(700,337,'Record the outcome' if en else '记录兑换结果',16,'#93a5b5'),text(34,363,'Threshold: 20 min' if en else '示例阈值：提前 20 分钟',16,'#93a5b5')]
        moves += [packet(left,.03,.19,AMBER),packet(right,.29,.49,AMBER),packet(right,.61,.77,GREEN,True)]
        moves += [state(402,367,'CHECK',.03,.24,AMBER),state(402,367,'REDEEM',.29,.55,AMBER),state(402,367,'RECORDED',.79,.97,GREEN)]
        footer=('Existing Codex credits only. Reset eligibility is decided upstream.' if en else '仅使用已有 Codex 重置券；能否重置由上游判定。')
    return head+''.join(body)+core()+''.join(moves)+text(34,454,footer,17,'#92a5b7')+'</svg>\n'

def main():
    for scene in ['request','response','fallback','queue','logical','autoreset']:
        for language in ['zh','en']:
            (OUT/f'flow-{scene}-{language}.svg').write_text(draw(scene,language))
    print('Built 6 independent SVG animations in Chinese and English')

if __name__=='__main__':main()
