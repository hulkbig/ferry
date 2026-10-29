# 渠道模型定时同步

Ferry 默认每小时读取启用渠道的模型目录，发现新模型后加入渠道，并自动建立缺失的同名逻辑模型。
可以在 **Providers → Edit → Automatic model sync → Catalog refresh** 选择每 30 分钟、每小时或关闭，
然后保存。关闭只停止定时同步；启用的渠道仍可使用 **Sync now** 立即同步一次。

Codex 客户端有效版本变化后，目录同步指纹随之失效；在默认启用的定时同步下，下一次调度即重新读取账户目录，自动建立新发现的同名逻辑模型。Codex 不受聚合渠道的自动创建数量上限影响。人工逻辑模型与自定义路由仍保留。只读发现预览不直接发布配置，预览选择在保存后生效。

编辑器显示最近一次结果、最后成功时间、下次运行时间，以及发现、新增、能力更新和自动创建逻辑模型的数量。
尚有未保存的修改时，请先保存再立即同步；同步期间输入的修改会保留。

## 同步内容

- 只读取模型目录，不发送推理或能力探测请求。
- 增加新模型；已有模型保留协议与路由，能力声明按下节优先级自动对齐。
- 复用同名逻辑模型的自动管理规则；人工维护的逻辑模型与路由不会被覆盖。
- 目录没有变化且能力无差异时不产生新的配置 revision。
- 同一渠道的所有启用连接都成功读取后才应用结果；某个连接失败时保留该渠道当前配置，避免只发布一部分目录。
- OpenRouter 官方渠道统一读取一次 Provider 级 `/api/v1/models` 目录，再把同一份 canonical 模型与能力元数据分发给
  Chat Completions、Responses 和 Anthropic 连接；只有有效凭据或认证形态不同才分别读取。

## 能力元数据来源

同步为每个发现的模型按以下优先级解析能力声明：

1. **上游目录元数据（权威）**：OpenRouter 官方域名的模型目录自带每模型元数据
   （`supported_parameters`、输入模态、上下文与输出上限）。它是该端点的权威契约，
   新模型按其声明录入，已有模型的能力声明会被对齐替换——例如此前只带保守
   `text` 能力入库的模型，会在下一次同步自动补齐流式、工具、推理、结构化输出、
   视觉与数值上限。OpenRouter 是 chat-completions 平台，所有列出的模型默认支持
   文本与 SSE 流式。
   OpenRouter Anthropic 兼容目录偶尔会把 callable slug 显示成
   `anthropic/<vendor>/<model> [1m]`；同步只对这种明确的 transport 前缀形式归一为
   `<vendor>/<model>`，不会改写真实的 `anthropic/claude-*` 模型 ID。
2. **社区聚合目录（只增不减）**：上游只返回模型 ID 时，从
   [models.dev](https://models.dev)（GitHub 开源项目 [sst/models](https://github.com/sst/models)）
   的跨平台聚合数据填充默认能力；同一模型 ID 在多个平台取能力并集，保持同名模型能力一致。
   该数据缓存 24 小时并自动更新；失败时静默降级，不影响同步本身。对已有模型只做加法
   （补齐缺失的能力位和数值上限），绝不减去已配置的能力，避免社区数据滞后造成误伤。
3. **保守协议默认值**：两层元数据都没有该模型时，沿用现有协议的保守默认
   （文本协议仅 `text`）。额外能力仍需可信声明或显式验证。

Codex OAuth 渠道的新模型按远程目录元数据推导能力：文本模型获得流式、工具、并行工具、推理与结构化输出的家族默认值，
上下文窗口取目录披露值；仅图像输入的模型按 Images API 契约处理。内置 Codex 目录仍对已知模型的基础能力保持权威替换。
Fast 的 priority 位来自账户目录 service_tiers，明确包含 priority 时开启，明确不含时关闭；
缺失或 null 保留已有声明。该更新覆盖已存在的模型，包括 `visibility=hide` 或其他不可发现条目；
只有 `visibility=list` 允许自动加入新模型。静态预置不会覆盖账户的 Fast 声明。
目录未覆盖的已配置模型保留原声明，但缺失不是支持证据，不能仅因希望出现 Fast 选项
人工写入 true。普通探测的 unknown 也不能升级为支持。发现缺乏账户/端点证据的人工
标记时，经草稿、模拟和 CAS 撤销，并同步客户端目录。请求档位、响应回显和实际加速须分开：
Codex OAuth 的 `service_tier` 回显仅作记录，`default` 或 `priority` 都不能单独确认实际执行档位，
也不能据此覆盖账户目录声明或推断延迟。
普通目录和社区聚合不提供这项账户权益证据，也不会抹掉人工配置的 priority。
客户端目录与 OpenCodeX 的配置同步见 [OpenCodeX Fast 能力同步](../skills/ferry/references/opencodex-fast.md)。

普通渠道使用已配置连接的模型目录和认证方式；Anthropic 目录会继续读取后续页，避免只看到默认的前 20 条。
Codex OAuth 读取已登录账户可见的远程目录，仅自动加入 `visibility=list` 的模型，不用本地静态清单过滤新名称。
已配置模型只要仍出现在账户目录中，也会按 Codex 目录契约补齐缺失的能力，包括流式、工具和推理；
该修复覆盖可见与隐藏条目，保留协议、路由及已有能力和数值上限。目录缺项时保留原声明，
Fast 仍按上文的独立账户权益规则更新。重复同步没有差异时不创建新 revision。
通过控制台或管理 API 增改已登录 Codex 连接的模型时，草稿保存也读取同一账户目录补齐能力，
使新模型在 Review/Simulate 时就具备完整声明，不必等定时同步。此次目录读取失败会拒绝该模型
编辑，保留原草稿和运行配置；无关编辑不依赖目录可用性。未登录的初始配置、目录未收录的
手工模型仍使用已有声明，不凭模型名称推断支持。
完整目录中的隐藏条目保留作已配置模型的能力证据；只有隐藏条目的有效目录也可以成功同步，
`discovered_models` 仍为 0。发现预览此时返回空列表，不退回套餐静态清单。
这个目录来自 [OpenAI Codex 官方客户端](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/endpoint/models.rs)
使用的 ChatGPT 后端接口，远程变动或账户权限变化会体现在同步状态中。

### 请求中的未知能力验证

运行态区分 `supported`、`unsupported` 和 `unknown`。为了兼容历史配置，能力布尔值为
`false` 本身只表示尚无支持声明；协议限制或有效的否定探测证据才表示明确不支持。
路由仍保留调用者要求，不删除工具、推理或流式参数来换取一次成功。

真实请求在选路前，使用该连接已经解析好的调用器检查所需能力，因此同样覆盖 Codex OAuth。
目录或配置已经声明支持的能力直接复用；未知的文本、流式、工具、推理、结构化输出才发送
固定内容的小型探测，不发送用户对话。推理验证要求实际推理事件或 reasoning token 用量，
仅返回正常文本不算成功。Fast 需要账户权益证据；图像生成与视觉不由此流程自动探测。
普通工具支持仍可通过现有串行兼容路径满足并行工具请求。

单次选路验证最多检查三个候选，总时限 20 秒；找到可用候选后停止，不预先探测未知的后备
路线。探测遵守连接并发准入，并额外限制全局同时运行两组验证；相同验证合并，并短暂缓存
15 秒以避免失败时反复触发。模拟路由、未授权请求、禁用或不可用连接不会发起验证。
这些合成请求可能产生少量上游费用，其数量单独记录，不计作用户请求的执行尝试。

自动证据保存在 SQLite，按 Provider、连接身份与凭据引用、端点、原生协议、模型和能力隔离；
成功证据有效期 30 天，明确拒绝有效期 7 天。旧的 Provider 端点探测证据仍作为兼容回退。
凭据替换或 OAuth 账户变更会清除相关 Provider 的旧证据；更换期间在途验证不能重新写回旧结果。
超时、网络故障、限流、没有推理证据，或一次未按工具/JSON 指令作答，均保持未知，
不会写入持久否定记录。认证、限流及连接故障仍进入正常的运行态冷却逻辑。

请求 trace 和模拟候选提供 `capability_status`；真实验证还记录
`capability_verification_error`、`capability_probe_requests`。未知能力的排除原因包含
`unverified:`，全候选未知归类为暂不可用，避免声称上游明确不支持。
能力修复不会启用已禁用的 Provider，也不会跳过其他渠道的熔断或账户限额。

### Codex Fast 的三层证据

目录请求的 `client_version`、`Version` 和 User-Agent 共用 `0.153.0`，满足所固定官方目录中
Astra 的最低版本要求。版本与模型级 Fast 基线分别维护在 `internal/domain/codex_models.go` 和
`internal/domain/codex_fast_baseline.go`；基线固定到 OpenAI Codex 提交
`cf6754e6828db447c288d72ca188d3dbc46feb56`，并记录原始 `models.json` 的 SHA-256。
基线只用于解释模型支持与检查客户端版本，不用于加入模型、修改账号权益或开启连接 Priority。
更新时核对该提交的 `service_tiers` 和 `minimal_client_version`，同时更新来源、摘要与版本回归检查。

手动同步成功响应的 `codex_catalog` 包含来源、请求版本，以及本次成功同步连接中各已配置模型的：

| 字段 | 含义 | 缺少证据时 |
| --- | --- | --- |
| `model_priority` | 固定官方目录中的模型级声明 | `unknown` |
| `account_priority` | 本次账号目录的 `service_tiers` 声明，包含隐藏模型 | `unknown`，不覆盖旧配置 |
| `connection_priority` | 本次同步后连接的 Responses Priority 声明 | 保留已有布尔配置 |

前两项使用 `supported` / `unsupported` / `unknown`。第三项为 true 仍须通过实际逻辑模型的
路由、协议与启用状态检查；客户端可用性以 `/v1/models` 和路由模拟为准。
这些诊断属于响应中的 revision 与时间戳所标识的一次同步，不作为持久化账号权益状态；
SQLite 仍保存配置及同步汇总，`GET /api/providers/{id}` 不重放这份目录诊断。

## 官方兼容端点与套餐

- DeepSeek 的 Anthropic 兼容接口位于 `/anthropic`，模型目录使用同一官方域名根路径的
  [`GET /models`](https://api-docs.deepseek.com/api/list-models)，以同一 API Key 的 Bearer 方式读取。
  目录路径调整不会改变连接的推理协议。
- 智谱 Coding Plan 的 `/api/coding/paas/v4/models` 已用现有账户实测返回非空目录。
  对同一 Provider、同凭据且已有 Coding 连接的已知官方协议路径，共用这个目录地址。
  这是实测兼容行为；[官方 OpenAPI](https://docs.bigmodel.cn/openapi/openapi.json) 当前未承诺该目录接口，
  所以仍保留错误状态、读取边界和定时重试。普通 API、不同凭据及自定义代理路径保持原语义。
- 豆包 Plan 的推理 API Key 无法访问已文档化的动态套餐目录。
  [官方 Plan 模型列表](https://github.com/volcengine/ark-cli/blob/main/skills/arkcli-plans/references/arkcli-plans-model-list.md)
  属于[控制面授权](https://github.com/volcengine/ark-cli/blob/main/skills/arkcli-auth/references/auth-modes.md)，
  需要单独的 STS 或 AK/SK 签名。可在普通渠道编辑器中选择 Volcengine Plan 目录，
  选择 Agent Plan 或 Coding Plan，并保存独立控制面凭据。仅配置 Plan 推理 Key 时仍显示目录不可用。

这些适配仅作用于明确的官方 HTTPS 域名和已知路径；不会把自定义代理的凭据转发到其他域名，
也不会以网页静态模型表替代账户目录。

## 豆包 Plan 独立目录

在 **Providers → Edit → Automatic model sync** 选择 Volcengine Plan 目录，并选择实际使用的套餐。
使用具有相应模型列表读取权限的独立控制面凭据：Access Key ID、Secret Access Key，
以及 STS 凭据必需的 Session Token。请在控制台密码输入框填写，不要发送到聊天、Issue 或日志中。
保存后仍使用同一套每 30 分钟 / 每小时计划，也可使用 **Sync now** 验证。

配置只保存密钥引用；密钥正文保存在已有本地 Secret Store，读取渠道配置不会返回密钥正文。
它必须与推理连接的密钥分开；`store:NAME` 和同名 `env:NAME` 也视为同一个密钥，不能复用。
凭据为空、格式错误、过期或权限不足时，同步会显示错误并保留当前模型。
STS 到期后需要由凭据提供者更新，Ferry 不会自行申请新 STS 或退回使用推理 API Key。

目录来源分别是官方的
[`ListArkAgentPlanModel`](https://api.volcengine.com/api-docs/view?action=ListArkAgentPlanModel&serviceCode=ark&version=2024-01-01)
和 [`ListArkCodingPlanModel`](https://api.volcengine.com/api-docs/view?action=ListArkCodingPlanModel&serviceCode=ark&version=2024-01-01)。
目录返回的新名称会加入启用的协议连接，沿用各连接原有协议并建立缺失的同名逻辑模型。
目录可见性不代表已经验证账户调用权限或模型全部能力。

## 全局调度开关

**Settings → General → Scheduled Model Sync** 是 Ferry 内置定时同步能力的全局开关（默认开启）。
暂停时所有 Provider 的到期同步停止派发；各 Provider 自己的 30 分钟 / 每小时间隔保持不变，
Provider 编辑器里的 **Sync now** 在暂停期间仍然可用。开关保存在控制面 SQLite 的运行时设置中，
服务重启后状态保留。

## 失败与恢复

目录不可用、认证失败、返回数据无效或超过读取边界时，现有模型和路由继续保留，界面显示具体同步状态。
暂不支持目录的原生渠道也按所选周期重试，不会转而进行推理探测。
修正渠道配置后，可保存并立即重试。

首次启用时立即安排同步，之后从完成时间起按所选间隔运行，调度检查最多有 30 秒偏差。
同步结果和下次运行时间保存在 SQLite。服务重启后恢复计划；中断的同步会重新安排。
网络读取结束后会再次检查配置和目录身份，避免把过期结果写入已经修改的渠道。

站内告警会单独报告同一 Provider 下、同一文本模型的连接级能力声明分歧；也会报告最近五分钟内
“所有连接均不符合条件、尚未发起任何上游请求”这一请求态异常。声明重新一致或异常请求滑出窗口后，
对应告警自动恢复。协议级能力探测仍按具体端点分别保存证据，不会被跨协议目录同步合并。

OpenRouter Contributor 等模型仍可能受账户级 data/training policy 限制。Ferry 会保留上游错误，
并对最近五分钟内明确包含 data policy 与 training/privacy 的拒绝生成 Provider 告警；它绝不自动开启、
关闭或修改 OpenRouter 的隐私/训练设置，必须由账户管理员在 OpenRouter 完成授权。

真实连接测试的文本输出预算为 32 tokens，避免低于部分兼容端点要求的 16-token 下限。
能力探测的基础文本预算为 64 tokens；若流正常结束却没有任何 canonical 内容，只用 256 tokens
有界重试一次，并把两次实际请求计入 `probe_requests`。其他上游错误不自动重试。

管理 API 字段、手动同步的高风险权限和返回结构见
[Ferry 管理接口](../skills/ferry/references/management-api.md#provider-model-catalog-sync)。

## 可发布的 Codex 请求版本

B 端「设置 → 通用 → Codex 客户端版本」可填写请求版本，通过「审核并发布」创建草稿、验证并以 revision CAS 发布。手动字段留空时使用最后一次自动版本，尚未自动更新时使用内置默认值（当前为 0.153.0）。配置字段 codex_client_version 与模型和路由一起保存在 SQLite 不可变配置版本中，可通过版本恢复；首次导入 YAML 也支持此字段。

目录查询参数 client_version、Version、User-Agent 以及 Codex 推理请求使用同一个已发布版本。变更会使已启用定时同步的 Codex 渠道重新到期；旧版本在途目录结果不能覆盖新配置，不同版本的目录读取也不会合并。暂停同步时需要恢复定时同步或手动「立即同步」。手动版本优先于自动跟随，不会被后台覆盖。

同步追加可见新模型，保留旧模型和人工路由；隐藏模型仅更新已配置模型的能力。仅 `internal/config/logical_model_policy.json` 白名单列出的聚合渠道（默认精确匹配 `openrouter.ai`）受自动创建上限限制（默认 10）。其他渠道，包括 Codex 和智谱，默认自动创建全部缺失的同名逻辑模型；已同步但过去受上限阻挡的模型会在下一次同步时补齐。白名单渠道超过上限时，上游模型仍同步，逻辑模型需手动选择；上限设为 0 也只影响白名单渠道。匹配 Provider 和 Connection 的端点域名，不依赖显示名或模型名前缀。修改白名单后需重新构建服务。缺少能力证据显示 unknown，不推断账号 Fast 支持。

### 自动跟随 GitHub 版本

同一卡片提供「自动跟随 GitHub 版本」开关和版本通道，均通过「审核并发布」保存，默认关闭以兼容已有手动配置。稳定版通道（默认）读取 openai/codex 的 GitHub latest release；「含预发布版」从最近 100 条 releases 中选择语义版本最高的 rust-v 标签，包含稳定版，忽略 draft 和无效标签。仅调整请求版本，不下载或执行 GitHub 二进制。

- 开关开启且手动版本为空时运行；填写手动版本则暂停检查，手动值始终优先。清空手动值并开启自动时，以原生效版本作为起点，避免首次检查前降级。
- 服务启动和每分钟扫描持久化计划，符合条件时立即检查，此后每小时检查。开关或通道变更在下一次扫描生效；重启保留已保存的检查计划。
- 自动发现的版本写入独立字段 codex_auto_version，使用现有验证、编译和 SQLite 发布流程。只升级、不降级、相同版本不产生配置修订；由预发布通道切回稳定版会等待高于当前版本的稳定版。
- 关闭开关保留最后一次自动版本；如需指定旧版本，填写手动版本。手动字段、开关、通道及自动版本都随配置修订恢复；恢复为自动开启的修订后，检查会继续。
- GitHub 请求有 20 秒超时及 8 MiB 响应上限；错误或限流不改变已生效配置，下小时重试。网络请求不持有配置锁，发布时复核 revision；期间发生用户修改则放弃本次结果，下次扫描重试。
- 设置页显示已发布的更新状态、上次/下次检查时间和错误；可先查看生效版本再排障。版本检查独立于模型同步开关，后续目录刷新仍遵循全局和渠道同步设置、自动逻辑模型上限。

自动化覆盖 Ferry 请求版本和既有服务端模型目录同步，不会改写桌面 OpenCodeX 配置，也不绕过上游账号可用模型或 Fast 能力证据。
