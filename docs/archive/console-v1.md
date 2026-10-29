# 旧控制台设计（v1）

> 历史设计文档，保留当时的 SmartAPI 名称。当前项目名为 Ferry；下文 Skill 链接已指向现行目录。

> 历史设计基线（v1.0）。其中未版本化 `/api/*` 业务接口已经下线并返回 `404`，不得用于新实现。
> 当前产品结构与完整 V2 契约见仓库 [`README.md`](../../README.md)、
> [`web/console/API-CONTRACT.md`](../../web/console/API-CONTRACT.md)
> 和 [`skills/smartapi/references/management-api.md`](../../skills/ferry/references/management-api.md)。

版本：v1.0
对象：单管理员或小团队运维者

## 1. 产品结构

控制台只保留六个一级页面：

| 页面 | 核心责任 |
| --- | --- |
| 概览 | 全局请求、成功率、Token、成本、渠道与模型分布 |
| 请求日志 | 请求、执行尝试、命中策略、Fallback 链和缓存命中证据 |
| 策略编排 | 有序规则、单策略启停、正文版本与单策略回滚 |
| 渠道管理 | 上游端点、凭证、模型别名、协议、能力、渠道组和运行状态 |
| 消费方 | API Key 生命周期及与全局同口径的流量、失败和成本分析 |
| 系统设置 | SQLite 版本、运行快照和系统级回滚 |

协议、模型别名、能力和凭证全部属于渠道详情。渠道组是渠道的集合，也是策略路由目标；不存在第二套“资源池目标”。成本只出现在概览、请求和消费方分析，不提供独立计价页面。设置页不展示消费账号。

## 2. 渠道工作流

渠道列表只展示名称、启停状态、模型与协议摘要、渠道组、运行状态和入口操作。完整配置放在独立的新建页或详情页，并按以下标签拆分：

1. 基本信息：渠道 ID、类型、Base URL、默认计划。
2. 模型与协议：上游模型、客户端别名、原生协议、能力契约。
3. 凭证：SQLite 密钥引用、明文更新、Header、前缀和可选 Query 参数；明文不回显。
4. 渠道组：策略可选择的渠道集合。
5. 运行分析：编译后的实例、健康信息与连通性测试。

新渠道默认停用。保存只修改配置，启停只由独立 Toggle 完成；不存在“保存并激活”。删除前必须验证没有启用策略引用该渠道。

## 3. 有序策略模型

### 3.1 首条命中

启用策略按优先级从上到下执行，首条满足 WHEN 的策略决定本次请求的 Route。每条策略独立保存、启停、版本化和回滚，规则集不存在整体“激活”动作。

WHEN 支持：

- 消费方；
- 模型或客户端别名；
- 入口协议；
- 必需能力；
- 星期；
- 时区与时间窗口。

启用规则的最后一条必须是无条件 catch-all。命中顺序只能通过拖拽策略列表改变；拖拽不会修改策略正文版本。

B Console v3 将同一语义收敛为 Logical Model 的“默认路由 + 有序 Overrides”：启用的 Override 仍按顺序首条命中，未命中时直接使用默认路由，因此 Overrides 列表本身不需要再创建一条 catch-all。Override 可按消费方、入口协议、必需能力、星期以及带 IANA 时区的时间窗口匹配，并可调整默认路由优先级或替换为自定义路由。

### 3.2 Route 与 Fallback

Route 是从主路由到多个 Fallback 的有序步骤。每一步只能选择一个渠道或一个渠道组；步骤可拖拽排序。故障是否进入下一步由当前步骤的 `recover_on` 决定。

允许恢复的故障类型：

- `connect_failure`
- `rate_limit`
- `provider_5xx`
- `quota_exhausted`
- `ttft_timeout`
- `stream_broken`

协议无法无损转换、以及仅对当前目标不兼容的参数，是在响应提交前发现的兼容性事实，不属于可配置的上游故障恢复；调度器会自动尝试下一 Route，避免返回失真内容。鉴权错误、客户端级参数错误和响应形状错误始终 fail closed，不能通过配置降级到其他渠道。渠道组内部候选耗尽后，调度器才进入下一 Route 步骤。

### 3.3 版本与哈希

策略正文仅由 Match 与 Route 构成。正文使用规范化 JSON 计算完整 SHA-256：集合字段先排序去重，界面名称、说明、优先级、启停状态、revision 和时间戳均不参与正文哈希。

因此：

- 修改 WHEN 或 Route 才生成新的 immutable revision；
- 重命名、排序和 Toggle 不产生正文 revision；
- 启用策略的 `ID + priority + revision + content_hash` 形成策略索引哈希；
- 相同正文哈希复用编译结果；
- 每个运行快照使用独立的场景缓存，键包含索引哈希、消费方、模型、协议、能力和当前时间分钟；
- 渠道配置变化会生成新运行快照，不能错误复用旧场景结果。

## 4. SQLite 权威模型

运行时不读取 YAML。旧配置只能在空控制库上显式导入一次。

| 表 | 责任 |
| --- | --- |
| `control_config_versions` | 其他控制面配置的事务版本信封 |
| `policies` | 策略身份、显示元数据和当前正文 revision |
| `policy_revisions` | 不可变 Match、Route、正文哈希与创建时间 |
| `policy_set_versions` | 与控制面版本绑定的有序规则集和索引哈希 |
| `policy_set_members` | 规则集顺序、启停状态和引用的正文 revision |
| `local_secrets` | 上游密钥与 OAuth 令牌 |
| `ledger.db` | 请求、尝试、决策追踪、Token、失败和成本 |

加载活跃配置或回滚时，路由策略从规范化策略表和 immutable revision 重建；配置 JSON 中的策略副本不能覆盖它们。无条件旧 DAG 策略在数据库打开时幂等迁移到规范化策略表；含条件规则的旧策略不会被压平成 catch-all。V2 Logical Model 投影会保留能够无损表达的消费方、协议、星期、时区时间窗和能力条件；无法无损表达的表达式、目标运行状态或排名/分层动作会形成阻塞迁移问题，在问题解决前继续由旧 Planner 执行并拒绝把该配置切换为 V2 原生事实源，避免静默丢失语义。

## 5. 消费方与观测

消费方就是客户端 API Key。列表默认只显示掩码 Key、状态、默认模型和窗口用量；详情页才提供显式查看、复制、轮换、删除以及流量分析。轮换后旧 Key 立即失效，删除消费方不删除历史统计。

请求和追踪记录至少包含：

- `policy_id`、`policy_revision`、`policy_hash`；
- `policy_index_hash`；
- `policy_compile_cache_hit`、`policy_scene_cache_hit`；
- `fallback_count` 与每次 Attempt 的失败分类；
- Token、延迟、现金成本、影子成本、订阅分摊和配额消耗。

这些字段使同一个固定场景为何命中某条策略、是否复用缓存、为何发生 Fallback 都可追溯。

## 6. 管理接口

| 方法 | 路径 | 语义 |
| --- | --- | --- |
| GET / POST | `/api/policies` | 列出有序规则；创建一条默认停用规则 |
| GET / PATCH / DELETE | `/api/policies/{id}` | 单策略读取、保存正文、删除 |
| PATCH | `/api/policies/{id}/enabled` | 独立 Toggle |
| POST | `/api/policies/reorder` | 保存完整顺序，不生成正文 revision |
| GET | `/api/policies/{id}/revisions` | 单策略 immutable revision 历史 |
| POST | `/api/policies/{id}/rollback` | 仅回滚该策略正文 |
| GET / POST | `/api/channels` | 渠道列表与新建 |
| GET / PATCH / DELETE | `/api/channels/{id}` | 渠道详情、保存、删除 |
| POST | `/api/channels/{id}/test` | 测试连通性 |
| GET / POST / PATCH / DELETE | `/api/consumers...` | Key 生命周期与消费方配置 |
| GET | `/api/consumers/{name}/analytics` | 单消费方流量与成本分析 |

写接口支持 `expected_version`，版本过期返回 `409`；单策略正文保存还支持 `expected_revision`。服务端验证、SQLite 写入和运行快照发布构成一个事务性流程，发布失败时恢复上一活跃版本并丢弃未实际服务的候选版本。

## 7. 视觉和交互准则

- 浅色中性背景、蓝色主操作、绿色运行状态、红色危险动作。
- 页面只保留一个主标题和一个主操作；解释性内容保持短句。
- 列表高密度但只展示关键摘要；复杂编辑都进入二级页。
- 颜色必须同时配文字；缺失值显示 `—`，不得推断为零。
- 破坏性操作二次确认；成功用 Toast，失败保留明确错误。
- 桌面端拖拽排序；窄屏切为单列并保留可点击操作，拖拽不是唯一入口。
- Demo 使用固定、可对账数据，所有写请求在网络前拒绝。

## 8. 验收目标

- [x] 一级导航只有六个责任页。
- [x] 渠道列表简洁，新建和详情为独立页面。
- [x] 协议、别名、能力、凭证和渠道组只在渠道域配置。
- [x] 保存与 Toggle 分离；新渠道、新策略默认停用。
- [x] 策略按顺序首条命中，并要求最终 catch-all。
- [x] 策略正文独立 revision、SHA-256、回滚和 CAS。
- [x] Fallback 按步骤和故障类型执行，鉴权与参数错误 fail closed。
- [x] SQLite 规范化策略表是路由事实源。
- [x] 编译缓存、场景缓存和观测字段可验证。
- [x] 消费方 Key 默认掩码，完整生命周期位于详情页。
- [x] 成本只在全局、请求和消费方分析出现。
- [x] 控制台无 YAML 编辑器，设置页无消费账号。
- [x] 品牌统一为 SmartAPI。
