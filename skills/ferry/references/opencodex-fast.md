# OpenCodeX Fast 能力同步

Fast 在这条 OpenCodeX 链路中默认发送 OpenAI Chat / Responses 的 service_tier=priority。
OpenAI [Fast mode 文档](https://developers.openai.com/api/docs/guides/priority-processing)
（2026-09-20 核对）也支持 fast 别名；Ferry 对这两个请求值执行同一 priority 能力门禁，
普通 Chat / Responses 转发保留归一化后的值，Codex 边界将 fast 转为 priority。
同步工具发布 priority，兼容 OpenCodeX 默认映射。
它需要具体上游端点或 OAuth 账户声明支持；模型名称、普通请求成功、延迟较短都不是证据。
目录缺失或普通探测 unknown 都不是支持证据。不能仅因用户希望显示 Fast 就人工写入 true；
账户目录声明与真实请求的返回档位必须分开记录。

## 能力来源与转发

Ferry 用连接模型的 capabilities.priority 保存声明，默认 false。
Codex OAuth 模型同步读取账户目录的 service_tiers：明确包含 id=priority 时开启，
明确列出其他档位或空数组时关闭；字段缺失或 null 表示未知，保留已有声明。
新模型与已存在的预置模型都会更新这一位，静态 Codex 预置不会将账户声明覆盖掉。
普通 API 上游需按端点的可信契约人工声明；能力探测对此返回 unknown，
不会用试发请求或计时来推断 Fast。
Codex 目录仅将 visibility=list 的条目加入发现列表和自动新增模型；完整目录中的隐藏条目
仍可更新已配置模型的 Priority。只有隐藏条目的有效目录不会触发套餐静态列表回退。
模型未被目录覆盖不等于明确拒绝 Priority。

Ferry 目录请求版本为 0.153.0，Fast 模型基线固定到官方 OpenAI Codex
`cf6754e6828db447c288d72ca188d3dbc46feb56` 的 models.json，记录原文件 SHA-256。
基线仅说明模型级支持，不替代账号目录。手动同步响应的 codex_catalog 分别给出
model_priority、account_priority（supported / unsupported / unknown）和
connection_priority（同步后的连接声明），并附来源与请求版本。
该诊断仅表示本次同步的证据；路由可用性继续由 /v1/models 和模拟确认。
具体字段见 [管理 API](management-api.md#provider-model-catalog-sync)。

带 priority 或 fast 的请求将它作为调度必需能力，没有声明支持的目标不能入选。
Chat、Responses、Codex 转发保留该字段（Codex 归一化 fast 别名）；无法表达它的跨协议转换会失败。
其他 OpenAI 档位原样保留，不代表 Ferry 已验证这些档位的账户权益。

GET /v1/models 的每个逻辑模型发布 supported_service_tiers，分别列出
openai-chat 和 openai-responses 的可用档位。只有默认路由及所有可能影响该协议的启用规则
都存在支持 priority 的目标时才发布 priority。未声明、目标停用、禁止转换和 exact-only
限制均参与判断；瞬时健康和剩余额度不影响目录。目录描述配置能力，不承诺本次请求可用或延迟。

## OpenCodeX 2.43.0 接入

经本机安装源码核对，2.43.0 的目录生成会按 Provider 配置重新计算 Fast 支持，
不能仅凭服务端模型目录自动发布 Fast。Skill 内置的
[同步脚本](../scripts/sync_opencodex_fast.py) 将 Ferry 的明确声明转换为
精确模型 ID 的 modelSupportsServiceTier，并通过受支持的 ocx config set / ocx sync 生效。
它不修改已安装的 OpenCodeX 包，也不按模型名称预设 true。

服务端部署此修复后，先完成 Codex Provider 的一次目录同步（或等待已启用的自动同步），
再在已有 OpenCodeX Provider 配置的机器运行。先从本次加载的 SKILL.md 路径确定
Skill 目录，不依赖调用者的工作目录：

    FERRY_FAST_SYNC="<loaded-skill-directory>/scripts/sync_opencodex_fast.py"
    python3 "$FERRY_FAST_SYNC" --provider ferry
    python3 "$FERRY_FAST_SYNC" --provider ferry --apply

不带 --apply 只显示变更，带 --apply 保存模型声明并刷新客户端目录。FERRY_API_KEY 必须由已授权的
秘密管理流程注入进程，使用对应网关的消费者 Key；Management Token 不能替代它。
使用你选用的秘密管理工具将消费者 Key 注入进程环境；缺少时先完成凭据配置，
不要将 Key 写入仓库或日志，也不为了同步目录自行创建 Key。仅同步客户端目录无需调用管理 API 或发起推理。
如果需要手动触发服务端 Provider 目录同步，先按 [管理 API 约定](management-api.md)
读取当前权限和 Provider 状态；已授权后可调用 POST /api/providers/{id}/models/sync
（无请求体），带 --confirm-high-risk，仍受服务端高风险门禁约束。
脚本从 ocx config get 的脱敏结果读取网关地址，只向该地址发送模型目录 GET，不发推理请求。
可用 --ocx 指定 CLI 可执行文件路径。

脚本支持 OpenAI Chat / Responses 及逐模型协议覆盖；有自定义 fastWire 时必须仍发送
service_tier=priority。禁止 Provider 全局 supportsServiceTier=true，避免新模型未经声明就显示 Fast。
服务端未发布扩展字段、目录为空、请求重定向或读取期间 Provider 被修改时，脚本报错并停止写入。
已删除模型的旧 true 覆盖会改为 false。保存后 ocx sync 失败会明确报告，可重跑完成刷新。

该脚本是一次性同步工具。上游权益或路由变化后需再次运行；服务端始终独立执行能力检查，
过期的客户端 Fast 开关不会绕过路由约束。现有 API-key scope 等访问限制仍在实际请求时执行。

## 缺失证据与撤销人工标记

目录未覆盖的模型表示未确认。普通 API 上游的人工声明也必须有该具体端点的可信契约，
不能以用户希望开启、模型同名、其他账户可用或请求成功作为依据。
优先读取实际账户目录；不要为穿过路由能力门禁而临时把生产配置改为 true。

发现缺乏依据的人工标记且用户已授权撤销时：

1. 读取当前权限、Provider 和配置版本，创建草稿并保存 base_revision。
2. 从草稿 intent.connections 读取持久化 models，仅将目标模型的
   capabilities.priority 改为 false。PATCH /api/providers/{id}/connections/{connection_id}?draft_id={draft_id}
   的 models 会替换完整数组，必须保留其他模型、协议、能力和 plan_id；
   不要把运行态视图的有效兼容能力写回原生契约。回读草稿并核对仅目标位变化。
3. validate，并分别 simulate 普通 openai-responses 流式请求和
   required_capabilities.priority=true 请求；确认普通路由仍可用，撤销目标不再参与 Priority。
4. 沿用该目标的已有授权，以 expected_revision CAS apply 并回读。409 先对账，
   不盲目重放。账户同步后再次核对，然后运行客户端同步预览、--apply 和幂等预览。

记录标记原始来源、撤销版本、响应回显和客户端结果。有真实目录证据的声明不因
一次 default 响应被改称人工标记；保留目录事实，Codex OAuth 回显不作为执行档位证明。
不要开启原本停用的账户或更改默认模型。

## 实际档位验收

对已有证据且路由允许 Priority 的目标，发送一个最小真实流式 Responses 请求：
指定 model、input="Reply exactly OK."、max_output_tokens=128、store=false、
stream=true、service_tier=priority，使用当前账户支持的推理参数。
通过已加载 Skill 的 inference 客户端发送并读取 SSE；记录时间、逻辑模型、实际连接
（可由请求台账回读）、响应 ID、完成状态、请求及返回档位。

请求被接受不证明实际使用 Priority。公共 OpenAI API 的完成响应档位可按其契约确认；
Codex OAuth 的回显不是实际执行档位的权威证据，default 和 priority 都只能记录为未确认，
不能由 default 推断 Fast 被降级。缺失字段、失败和取消也不能确认。Codex 流式及非流式
均保留实际回显；不要改写原值，也不能用耗时推断。

Ferry 请求台账的 service_tier_evidence 分开记录实际出站档位、回显来源和确认结果。
响应头 X-Ferry-Response-Tier-Authority 描述实际选中上游的回显契约，模型目录的
response_tier_authority 则保守合并所有可能路由，两者不能互相替代。

新版 OpenCodeX 支持 responseTierAuthoritative 配置项后，可显式同步：

    python3 "$FERRY_FAST_SYNC" --provider ferry --sync-tier-authority
    python3 "$FERRY_FAST_SYNC" --provider ferry --sync-tier-authority --apply

默认不写新字段，保留旧客户端兼容性。只有所有模型所用协议的目录值均为 authoritative
才写 true；Codex、混合或未知来源写 false。它与 Fast 能力声明独立，不会因回显不可信就
移除账户已有声明，也不会替未声明的模型开启 Fast。脚本回读确认字段确实保存后才继续；
旧客户端不支持或旧服务器缺失元数据时失败并报告。当前 OpenCodeX 配置修复不会自动
消费逐响应头，路由变化后需重新同步。完整契约见工程 docs/service-tier-evidence.md。

OpenAI 公共 API 文档中的 fast 别名不代表所有 Codex OAuth 端点都接受。保持本桥接
工具的 priority 映射；遇到 Unsupported service_tier: fast 应核对具体端点，
不因客户端显示 Fast 就改发 fast，也不将上游降档记录为网关已经实现加速。

`--provider` 必填，传入现有 Provider ID；不会随产品改名。原 `smart-api` ID 可以继续使用。配置的数据 URL 必须与所选 Provider 的 origin 一致。
