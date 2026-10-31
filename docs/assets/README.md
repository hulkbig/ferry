# README 配图

首页使用一张抽象主视觉、六张流程动画和两张功能说明图。主视觉传达请求持续向前的感觉，功能图解释等待、切换、排队和 AI 管理的具体关系。

| 位置 | 配图 | 作用与内容 | 表达边界 |
| --- | --- | --- | --- |
| 首屏 | [请求流](request-flow.png) | 白色主路径继续前进，琥珀色分支表示暂时等待；形成克制的科技视觉 | 抽象概念图，不是请求日志或性能证据 |
| 上手步骤之后 | 请求、返回、fallback、排队、逻辑模型、AutoReset 六张中英文 SVG 动画（`flow-*.svg`） | 分别呈现客户端选路、流式返回、备用账号及容量释放 | 独立场景示意，不承诺所有组合均通过真实验证 |
| 三项连续性能力之后 | [等待、接力、排队](continuity-zh.svg) · [English](continuity-en.svg) | 区分限流后等待恢复、故障后切换备用、并发满时排队 | 三个独立场景，不表示每个请求都按三步串行执行；恢复有配置与预算约束 |
| AI 友好说明之后 | [AI 原生管理](agent-native-zh.svg) · [English](agent-native-en.svg) | Agent 经 Skill 和管理 API 读取证据、准备草稿、审阅应用、回读确认 | 是管理流程示意，不是产品截图或真实操作回执；应用仍需授权与版本检查 |

## 视觉约定

- 保留现有 Ferry Logo。产品名不展开成船、码头、桥梁或海面插画。
- 主色使用石墨黑与暖白，薄荷绿表示可执行路径，琥珀色表示等待或需要处理的状态。
- 接入图按用户要求使用具体产品名与品牌标志，品牌标志仅用于识别接入对象，不表示合作、认证或完整兼容性验收。不编造性能曲线、成功率、客户评价或生产数据。
- 所有关键能力都有可独立阅读的正文；图片提供辅助解释。中文、英文说明图分别提供，README 保留替代文本。
- 功能图使用可编辑 SVG，禁止外链字体、脚本或远程资源。主视觉使用本地 PNG，避免依赖图片 CDN。

## 来源

`request-flow.png` 于 2026-10-24 使用内置 imagegen 工具生成，未使用外部参考图。最终[生成提示词](request-flow.prompt.txt)随资产保存。功能图为本项目编写的 SVG；文字内容根据当前路由、并发和管理 API 约定整理。接入图于 2026-10-26 改为暗色节点与连接线构图，参考 [Magpie 接入图](https://github.com/yetone/magpie/blob/main/site/public/img/architecture.svg)的信息密度；未复制其构图源码。品牌图形取自固定版本 `@lobehub/icons-static-svg@1.100.0`，原件位于 `vendor/lobe-icons/`，MIT 许可与校验记录位于 `../../LICENSES/LobeIcons/`。

这些新增资产只被文档引用，不替换运行中的控制台图标。控制台及其他资产的审查记录见[资产来源](../asset-provenance.md)。

## 后续产品截图

公开前补齐控制台总览、一次恢复请求的详情、模型与路由三张截图。使用隔离演示实例和虚构数据，并在图注标注“示例数据”；真实页面布局与操作需来自当前产品。截图不含内网域名、真实账号、Key、用户正文或生产费用。每张截图记录源提交、页面、语言和主题。尚未拍摄的图片不使用原型图冒充。

## 动态流程图

`scripts/build_gateway_diagrams.py` 生成 `flow-{request,response,fallback,queue,logical,autoreset}-{zh,en}.svg`。六张图分别绘制右侧节点，使用浏览器原生 SVG 动画，无 GIF、JavaScript、外链字体或远程资源。官网内联同一份 SVG，提供独立暂停、重播及离开视口暂停；减少动态效果模式默认静止。README 通过图片引用，实际动画表现取决于阅读器，静态节点仍可阅读。

Hermes 使用通用 H 字母标记，不冒充其官方 Logo。其余品牌形状沿用上述固定版本与许可证。修改后重新运行生成脚本，并验证中英文、两种主题、窄屏和动画状态。

## Performance figures (1.1.0)

`perf-*-zh.svg` and `perf-*-en.svg` cover deployment, throughput, CPU/request, memory working set, ledger storage and test topology. Regenerate with `python3 scripts/build_performance_figures.py`. Numerical sources are `docs/benchmarks/2026-10-30/summary.json` and the documented fresh-volume disk test. They are static, accessible SVGs with titles/descriptions; labels state the unequal resource/features and short-run limits.

The Skill and switching-policy figures use the same 960×480 frame and palette as the flow diagrams. Peak CPU figures use `peak_cpu_percent` from the published summary; interval evidence is in `cpu-intervals.json`.
