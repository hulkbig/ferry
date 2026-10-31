#!/usr/bin/env python3
"""Regenerate bilingual benchmark SVGs from the published summary; no network."""
from pathlib import Path
import json, html
pub=Path(__file__).resolve().parents[1]
summary=json.loads((pub/'docs/benchmarks/2026-10-30/summary.json').read_text())
# Vector figures are generated from the published numerical summary, never hand-painted bars.
assets=pub/'docs/assets';bg='#10151c';panel='#171e26';ink='#e9eef4';muted='#92a5b7';cyan='#79d8ff';blue='#719c8b';orange='#c79d63'
def txt(x,y,s,size=22,color=ink,weight=400):return f'<text x="{x}" y="{y}" fill="{color}" font-size="{size}" font-weight="{weight}">{html.escape(str(s))}</text>'
def rect(x,y,w,h,color=panel,r=15):return f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{r}" fill="{color}"/>'
def svg(name,title,subtitle,body,h=680):
 s=f'<svg xmlns="http://www.w3.org/2000/svg" width="1200" height="{h}" viewBox="0 0 1200 {h}" role="img" aria-labelledby="title desc"><title id="title">{html.escape(title)}</title><desc id="desc">{html.escape(subtitle)}</desc><rect width="1200" height="{h}" rx="24" fill="{bg}"/><g font-family="Arial, PingFang SC, Microsoft YaHei, sans-serif">'+txt(48,65,'FERRY / 1.1.0',18,cyan,700)+txt(48,115,title,34,ink,700)+txt(48,150,subtitle,17,muted)+body+'</g></svg>'
 (assets/name).write_text(s)
def frame(name,title,body,footer):
 ident=name.removesuffix('.svg')
 out=f'<svg xmlns="http://www.w3.org/2000/svg" width="960" height="480" viewBox="0 0 960 480" role="img" aria-labelledby="{ident}-title {ident}-desc"><title id="{ident}-title">{html.escape(title)}</title><desc id="{ident}-desc">{html.escape(footer)}</desc><g font-family="-apple-system,BlinkMacSystemFont,Segoe UI,Arial,PingFang SC,Microsoft YaHei,sans-serif"><rect x="1" y="1" width="958" height="478" rx="20" fill="#10151c" stroke="#303c47"/><path d="M32 68H928" stroke="#28343f"/>'+txt(34,43,title,20,'#e9eef4')+'<text x="925" y="42" font-size="12" fill="#778a9d" text-anchor="end" letter-spacing="3">FERRY</text>'+body+txt(34,449,footer,15,'#92a5b7')+'</g></svg>'
 (assets/name).write_text(out)

def metricplot(lang,metric,title,unit,lower):
 en=lang=='en';body=''
 for col,case in enumerate(['json-1k-c32','json-64k-c32']):
  x=34+col*456
  body+=f'<rect x="{x}" y="96" width="436" height="306" rx="13" fill="#171e26" stroke="#34404b"/>'
  body+=txt(x+20,129,'1 KiB' if col==0 else '64 KiB',20,'#e9eef4')
  data=[r for r in summary if r['case']==case];mx=max(r[metric] for r in data)
  for i,r in enumerate(data):
   y=177+i*78;value=r[metric];color='#79d8ff' if i==0 else '#92a5b7'
   label='Ferry' if r['product'].startswith('Ferry') else 'CLIProxyAPI' if r['product'].startswith('CLIProxyAPI') else 'sub2api + PG / Redis'
   number=f'{value:,.0f}' if metric=='success_rps' else f'{value:,.1f}%' if metric=='peak_cpu_percent' else f'{value:.1f}'
   body+=txt(x+20,y,label,18,'#e9eef4')+f'<text x="{x+416}" y="{y}" text-anchor="end" fill="{color}" font-size="20">{number}</text>'+rect(x+20,y+13,max(2,396*value/mx),10,color,5)
 footer=('Peak CPU · 100% = 1 core · sub2api includes PG / Redis' if en else '峰值 CPU · 100% = 一个逻辑核 · sub2api 含 PG / Redis') if metric=='peak_cpu_percent' else ('Peak working set · MiB · sub2api includes PG / Redis' if en else '峰值内存工作集 · MiB · sub2api 含 PG / Redis') if metric=='peak_working_set_mib' else unit
 frame(f'perf-{metric}-{lang}.svg',title,body,footer)

for lang in ['zh','en']:
 en=lang=='en'
 metricplot(lang,'success_rps','Measured throughput' if en else '吞吐对比','successful requests / second',False)
 metricplot(lang,'peak_cpu_percent','Peak CPU usage' if en else '峰值 CPU 占用率','CPU %',True)
 metricplot(lang,'peak_working_set_mib','Memory under load' if en else '内存占用对比','MiB working set; sum of component peaks for sub2api',True)
 body=''
 cards=[('ONE PROCESS','Go + SQLite','No external database' if en else '无需外部数据库'),('TWO MODES','Console / Headless','Same routing and audit' if en else '路由、鉴权与审计继续保留'),('PULL & RUN','Docker image','AMD64 + ARM64' if en else 'AMD64 与 ARM64')]
 for i,(k,v,d) in enumerate(cards):
  x=48+i*375;body+=rect(x,190,355,160)+txt(x+24,225,k,17,cyan,700)+txt(x+24,275,v,25,ink,700)+txt(x+24,315,d,19,muted)
 for x,label in [(48,'CLIENTS'),(455,'FERRY'),(862,'UPSTREAMS')]:body+=rect(x,405,290,115)+txt(x+25,450,label,22,cyan,700)+txt(x+25,488,{'CLIENTS':'Agent / App / CLI','FERRY':'Route · Wait · Recover','UPSTREAMS':'API keys / OAuth'}[label],18,ink)
 body+='<path d="M338 462 H445 M745 462 H852" stroke="#4bd9c4" stroke-width="4"/><path d="m432 452 13 10-13 10 m394-20 13 10-13 10" stroke="#4bd9c4" fill="none" stroke-width="3"/>'
 body+=txt(48,580,'Tools, streaming, usage and decision traces stay connected.' if en else '工具调用、流式响应、用量与决策轨迹，都有完整上下文。',23,ink)
 body+=txt(48,625,'Recovery follows configured budgets and protocol compatibility.' if en else '恢复受时间、次数和协议兼容性约束，不承诺每次都能成功。',18,muted)
 svg(f'perf-overview-{lang}.svg','Deployment architecture' if en else '部署结构','Self-hosted AI gateway · Apache-2.0 · release candidate',body)
 body='';vals=[('Decision traces',31.27,cyan),('Requests',3.586,blue),('Attempts',2.098,orange),('Indexes & other',2.819,muted)];x=48
 for label,v,c in vals:w=v/39.773*1100;body+=rect(x,215,w,64,c,0);x+=w
 for i,(label,v,c) in enumerate(vals):body+=rect(48,320+i*52,16,16,c,3)+txt(85,335+i*52,label,22)+txt(730,335+i*52,f'{v:.2f} MiB',22,c,700)
 body+=txt(48,565,'6,400 requests · ~39.8 MiB ledger · ~6.5 KB added per request',24,cyan,700)
 body+=txt(48,610,'Fresh-volume Mock run; audit retained; debug bodies OFF.',19,muted)+txt(48,642,'Storage footprint is not physical writes / IOPS. Other routes can cost more.',17,muted)
 svg(f'perf-disk-{lang}.svg','Audit has a storage cost' if en else '账本磁盘占用','Fresh volume · same 6,400 requests · no history deletion',body)
 body=''
 for i,(head,lines) in enumerate([('LOAD GENERATOR',['2 vCPU','c32 / 200 requests per second','Validate content + status + SSE']),('GATEWAY UNDER TEST',['2 vCPU / 1536 MiB','One gateway at a time','CPU + memory + ledger counts']),('MOCK UPSTREAM',['2 vCPU','Delay / jitter / bytes / chunks','No paid or real upstream calls'])]):
  x=48+i*375;body+=rect(x,200,355,245)+txt(x+20,245,head,21,cyan,700)
  for j,line in enumerate(lines):body+=txt(x+20,295+j*44,line,17,ink)
 body+=txt(48,505,'1 KiB / 64 KiB JSON + SSE · ARM64 Linux Docker on Apple M1',23,ink,700)
 body+=txt(48,552,'Gateway features differ. sub2api also runs PostgreSQL and Redis.',20,muted)
 body+=txt(48,596,'Short runs: Ferry 3 x 15 s; CLIProxyAPI 3 x 12 s; sub2api 1 x 30 s.',19,muted)
 body+=txt(48,637,'No claims for real-model tokens/s, multi-account OAuth or long-term capacity.',17,orange)
 svg(f'perf-method-{lang}.svg','Test configuration' if en else '压测配置','2026-10-30 · fixed versions · synthetic credentials · isolated network',body)
# Feature diagrams share the same frame, palette and typography as flow-*.svg.
for lang in ['zh','en']:
 en=lang=='en'
 def box(x,y,w,h,label,sub=''):
  return f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="13" fill="#171e26" stroke="#34404b"/>'+txt(x+20,y+35,label,20,'#e9eef4')+(txt(x+20,y+64,sub,16,'#92a5b7') if sub else '')
 body=box(34,115,226,100,'AI Agent','Claude Code / Codex')
 body+='<path d="M260 165 H334" stroke="#425262" stroke-width="2"/><path d="m323 158 11 7-11 7" fill="none" stroke="#79d8ff" stroke-width="2"/>'
 body+=box(334,115,592,100,'Ferry Skill + Management API','Models · Routes · Accounts · Requests · Settings' if en else '模型 · 路由 · 账号 · 请求 · 设置')
 body+='<path d="M630 215 V250 H148 M630 250 H812 M148 250 V280 M370 250 V280 M590 250 V280 M812 250 V280" fill="none" stroke="#425262" stroke-width="2"/>'
 for i,(label,sub) in enumerate(([('Inspect','State & requests'),('Configure','Models & routes'),('Diagnose','Failures & quotas'),('Apply','Validate & read back')] if en else [('查状态','模型与请求'),('改配置','路由与账号'),('做诊断','故障与额度'),('执行操作','校验与回读')])):
  body+=box(34+i*226,280,212,100,label,sub)
 frame(f'perf-skill-{lang}.svg','Official Skill · Full functionality' if en else '官方 Skill · 全功能支持',body,'One management API for agents and the console' if en else '与控制台使用同一套管理 API')
 body=''
 inputs=[('Time windows','Schedule routes'),('Timeouts','First-output budget'),('Failures','Error-based recovery')] if en else [('时间策略','按时段选择线路'),('超时策略','设置首输出等待上限'),('失败策略','按错误类型触发恢复')]
 for i,(head,sub) in enumerate(inputs):
  y=100+i*100;body+=box(34,y,260,82,head,sub)+f'<path d="M294 {y+41} H340 V241 H382" fill="none" stroke="#425262" stroke-width="2"/>'
 body+=box(382,190,210,100,'Route policy' if en else '路由策略','Evaluate & select' if en else '按配置判断与选路')
 for i,label in enumerate(['Continue','Wait','Switch'] if en else ['继续当前线路','排队等待','切换备用线路']):
  y=100+i*100;body+=f'<path d="M592 241 H650 V{y+41} H700" fill="none" stroke="#425262" stroke-width="2"/>'+box(700,y,226,82,label)
 frame(f'perf-strategy-{lang}.svg','Configure when to wait or switch' if en else '切换策略自由配置',body,'Time windows · First-output timeout · Failure recovery' if en else '时间窗口 · 首输出超时 · 失败恢复条件')
print('Generated 16 bilingual figures from data and feature definitions')
