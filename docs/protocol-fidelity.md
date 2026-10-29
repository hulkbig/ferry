# 三协议保真与转换边界

本文说明 Ferry 对 OpenAI Chat Completions、OpenAI Responses 与 Anthropic Messages 的处理约定，分为两条路径：

1. 原生同协议：保留客户端请求和上游响应的原始协议内容；
2. 跨协议转换：尽量完成请求；无法保留的可选字段允许降级并留下记录，协议不支持或缺少必需能力时拒绝该路由。

## 设计参考

协议分层参考过 [One API 的固定版本](https://github.com/songquanpeng/one-api/tree/8df4a2670b98266bd287c698243fff327d9748cf)与 [New API 的固定版本](https://github.com/QuantumNous/new-api/tree/e468b73915e5028e9849de62c5018a0faa203012)。本文描述 Ferry 自身的行为；参考项目的能力不代表 Ferry 已实现或已验证。

## 原生同协议承诺

三个文本协议共同遵守：

- 逻辑模型映射为实际上游模型时保留原始请求体，只改写网关拥有的 `model` 和 `stream`；
- 只有实际发生协议转换时才移除原始请求体；
- 转发 `Anthropic-Version`、`Anthropic-Beta`、`OpenAI-Beta` 等功能协商 Header，但不转发客户端认证或租户身份；
- 非流式响应保留未知字段，并只改写对外模型名和网关虚拟响应 ID；
- 流式响应同时产生规范化事件供调度、计量和追踪使用，并把原始 SSE `event` / `data` 帧交给同协议客户端；
- OpenAI Responses 的上游 response/conversation ID 不直接暴露，客户端只看到 Ferry 虚拟 ID，续接时再映射到当前连接的原生 ID。

这意味着新增的供应商字段或 SSE 事件不会仅因 Ferry 的 DTO 尚未升级而从原生链路消失。

## 跨协议转换矩阵

| 能力 | Chat | Responses | Anthropic | 转换策略 |
| --- | --- | --- | --- | --- |
| 文本与 system/instructions | 支持 | 支持 | 支持 | 等价映射 |
| Base64 图片 | 支持 | 支持 | 支持 | 等价映射；远程媒体不自动下载 |
| 函数工具定义 | 支持 | 支持 | 支持 | `parameters` / `input_schema` 映射 |
| 函数调用与参数增量 | 支持 | 支持 | 支持 | 保留 call/tool ID 和流式参数顺序 |
| 多个 tool result | 支持 | 支持 | 支持 | 每个结果独立关联对应 tool ID；跨协议只承诺可等价表达的文本结果，Anthropic 结构化结果保留在 Anthropic 路径 |
| tool result 的冗余名称 | 兼容扩展 | 不需要 | 不需要 | `tool_call_id` / `call_id` / `tool_use_id` 是权威关联键；Chat 客户端同时回显的 `name` 在规范化时安全消除，不得导致候选预检失败 |
| 已完成回放项的 `id/status` | 不需要 | 支持 | 不需要 | Responses 回放项的 `id` 和 `status=completed` 是服务端身份/终态回显；保留 call ID 与内容后可安全消除。未完成状态仍要求原生路径 |
| tool result 后的用户内容 | 支持 | 支持 | 支持 | Anthropic 同一 user turn 中前置的结果块与后续文本拆成相邻 canonical 消息，保持结果关联和原始顺序 |
| tool choice | 支持 | 支持 | 支持 | `required` ↔ `any`，具名函数 ↔ `tool` |
| 并行工具 | 支持 | 支持 | 支持 | `parallel_tool_calls` ↔ `disable_parallel_tool_use` 反向映射 |
| temperature / top_p | 支持 | 支持 | 支持 | 在目标支持时等价映射；已知不接受某字段的 Provider 仅在其出站边界定向归一化 |
| top_k | 扩展字段 | 无标准等价 | 支持 | Chat ↔ Anthropic 仅在可证明等价时映射；到 Responses 拒绝 |
| max tokens | 支持 | 支持 | 支持 | 字段名映射，并受模型 `max_output` 能力门槛约束 |
| stop sequences | 支持 | 无标准字段 | 支持 | Chat ↔ Anthropic 映射；任何到 Responses 的转换拒绝 |
| reasoning/thinking | `reasoning_effort` / `reasoning_content` 扩展 | reasoning effort/text/summary | thinking budget 与带 signature 的 thinking block | 请求控制中的 effort ↔ budget 做确定性映射，不因上游“可能”返回原生状态而提前阻断；实际响应逐次验证，Responses reasoning text/summary 可保留原文映射到 Chat `reasoning_content`，而 signature、redacted/encrypted state，以及任何无 signature 到 Anthropic 的转换仍要求原生路径 |
| structured output | 支持 | 支持 | `output_config.format` / `json_schema` | schema 双向映射；目标资格仍依赖端点／模型能力证据。无 schema 的 `json_object` 到 Anthropic 记录提示词降级，`exact_only` 拒绝 |
| usage/cache usage | 支持 | 支持 | 支持 | 映射已建模计数；出现未建模 usage 语义时要求原生路径 |

## 渠道预设与转换默认值

控制台中的渠道预设不把供应商名字硬编码成能力结论。预设只完成三件可安全复用的事：填入可编辑的 Base URL、切到最可能的原生协议探测页、开启 OpenAI Chat / Responses / Anthropic 三种文本客户端入口。模型清单及其原生协议仍由真实端点探测或人工确认。

保存时，三个文本入口分别写入 Provider 的 `protocol_conversion` 策略；`openai-images` 无论界面状态如何都写为 `false`。运行时对同名模型先选择与客户端协议相同的健康原生连接，只有原生连接不可用时才选择允许转换的连接。转换前后的能力门槛、有损请求拦截和首包前失败恢复继续遵守本文其余约束，因此“预设开启转换”不等于允许静默降级。

## 必须留在原生路径的能力

以下内容没有可证明的三协议等价表达。Ferry 会把对应转换路由判为不兼容，而不是删除字段后继续请求：

- 未识别的顶层请求字段、内容块或 Responses input/output item；
- Anthropic `redacted_thinking`、`cache_control`、server tools 和未来新增的原生内容类型；
- Responses 内建工具、加密 reasoning、原生 conversation/context management；
- Chat/Responses 的音频、视频、文件、远程媒体和无法转换的 annotations/citations；
- 供应商专属 `extra_body`、未建模的采样或输出控制字段。

消息级 `name` 仍有一条明确边界：user/system/assistant 的参与者名称可能影响提示语义，因此只能在 Chat 原生路径保留；只有同时带 `tool_call_id` 的 tool result 名称属于可安全消除的重复元数据。

同协议端点仍可完整接收这些内容。若某模型只有其他协议端点，控制台应把该模型显示为“可自动转换”，但运行时仍以本请求的实际能力为准；含原生专属能力的单次请求不会被错误降级。

## 流式事件要求

Anthropic 输出必须遵守 `message_start → content_block_start → delta* → content_block_stop → message_delta → message_stop`。thinking block 的 signature delta 保留在原生链路；工具块同样必须完整 start/stop。

Anthropic 工具流中的起始 `input: {}` 是参数增量的占位，不会与后续 `partial_json` 直接拼接；如果起始帧已经给出非空完整对象，则之后再出现参数增量会被判为坏响应。无论哪种形态，工具结束前都必须得到一个完整 JSON object，Ferry 不补造 `{}`。

Chat 转换保持 tool call index 稳定，所有并行工具分别结束，最后输出 finish reason、usage 与 `[DONE]`。

部分 OpenAI Chat 兼容上游会把 `message.content` 或流式 `delta.content` 返回为非标准的 JSON 对象、嵌套文本包装或混合数组。Ferry 会在 Chat 出站边界把可识别的文本包装还原为文本；其余合法 JSON 以紧凑 JSON 字符串完整保留，并附加兼容性 warning，而不是把整次响应判为失败。只有响应体或 SSE 帧本身不是合法 JSON 时才继续按坏响应处理。

Chat 的 `stream_options.include_usage` 是显式输出契约：未请求时转换流不泄露 usage；请求时先输出 finish chunk，再输出 `choices: []` 的独立 usage chunk，最后才发送 `[DONE]`。上游 Chat 转换请求仍会主动收集 usage 供网关计量，但不会因此改变客户端可见帧。

Responses 转换保留 output item 的 added/delta/done 顺序、reasoning/text delta、function arguments 和 completed/incomplete/error 终态。reasoning summary 的 part added/done 是生命周期帧；转换到 Chat 时，summary text delta 按原文写入 `reasoning_content`，转换到 Anthropic 时仍因缺少 signature 而拒绝。未知的内容型事件在转换路径中触发明确错误；同协议路径按原帧输出。

三个协议的 SSE `data` 都必须是该协议规定的合法 JSON（Chat 的 `[DONE]` 除外）。同协议直通允许未知字段和未来新增的合法事件，但不转发无法解析的畸形帧。

## 能力门槛与降级策略

Ferry 默认采用尽力兼容策略：`Exact`、`Mapped` 和 `Degraded` 请求都可继续路由，除非目标协议明确不支持、缺少请求必需能力，或映射器无法构造有效请求。降级字段会写入请求账本与兼容性诊断，不能伪装成精确兼容。历史 `allow_degradation` 配置字段为兼容保留，不再控制默认行为。API Key 或 Logical Model 任一设置 `exact_only=true` 时采用 strictest-wins，只允许 `CompatExact`；Mapped 与 Degraded 请求在第一次上游尝试前拒绝。缺省 `false`。

| 请求要求 | 目标缺少能力时 | 禁止的隐式降级 |
| --- | --- | --- |
| text | 跳过目标，继续有序路由；无目标则 `unsupported_parameter` | 不返回空文本或伪造占位文本 |
| images | 只选择原生 OpenAI Images 生成/编辑端点 | 不把图片 API 混同为文本模型或 chat vision，不跨文本协议转换 |
| stream | 跳过不支持流的目标 | 不把非流响应缓冲后伪装成 SSE |
| tools | 跳过不支持工具的目标 | 不把工具定义注入 prompt，不把 tool call 当普通文本 |
| parallel tools | 先选择明确支持并行工具的 exact 目标；默认策略下，没有 exact 时可把已注册并验证的请求映射为串行 tools；`exact_only` 时拒绝映射 | 未注册策略不得串行化；映射只改变未来调用方式，不改写既有 tool result，不制造并发副作用 |
| reasoning | 优先选择明确支持 reasoning 且请求/响应均可保真的路径；否则尽力发送并记录映射/丢弃项 | 不伪造 signature、redacted/encrypted state；Responses summary 只映射为 Chat `reasoning_content`，不冒充带 signature 的 Anthropic thinking |
| structured output | 只选择原生支持目标 | 不靠 system prompt 假装 schema 保证 |
| vision | 只选择支持图片且媒体形态可转换的目标 | 不下载远程媒体或删除图片块 |

能力不匹配在调用前完成筛选，不产生上游请求。供应商在调用后才声明某参数不支持时，记为 target-scoped `unsupported_parameter`，在客户端尚未收到内容时自动尝试下一条路由；若没有兼容目标则返回明确错误。

协议预检只判断当前请求能否映射，不能因为上游响应“可能”包含原生 reasoning 状态就预先排除目标。reasoning 的响应保真在真实响应或首个可见流事件提交前检查；确实出现 signature、redacted/encrypted state 或 summary 等客户端协议无法表达的内容时，再按 `protocol_conversion` 回退。Chat/Responses 未指定输出上限而 Anthropic 出站需要必填 `max_tokens` 时，Ferry 使用运行时默认值，这属于未指定值的协议适配，不算客户端语义丢失。

控制台手工录入的自定义 Provider 模型只从协议默认值和显式声明开始，不会再按 GLM、Kimi 或其他模型名家族猜测 `stream`、`tools`、`reasoning` 等能力。Codex OAuth 是例外：其模型目录与线协议由 Ferry 自身维护，继续使用内置权威能力表。

对自定义 Provider，只有操作者明确选择的模型/原生协议组合会执行能力验证，`/models` 目录本身不构成协议或能力证据。文本验证必须收到至少一个有效内容事件和协议终态，才能声明 `text=true`、`stream=true`；有界探测还可分别确认 tools、parallel tools、reasoning、原生 structured output 与 vision，Images 端点只确认 `images`。Anthropic 结构化输出探测实际发送 `output_config.format`，以返回内容与终态记录支持／不支持／未知；不再按协议固定否定，也不因兼容厂商名称自动开启。`context_window` 与 `max_output` 不做边界猜测或试探，仍由可信目录或操作者声明。

能力证据按 Provider、端点指纹、上游模型、原生协议和具体能力精确缓存：支持证据保留 30 天，确定性不支持证据保留 7 天，端点变化或显式 `refresh=true` 会重新探测。5xx、限流、超时等瞬时故障只返回 `unknown`，不会写成不支持；控制台也只用明确的 `supported` / `unsupported` 更新能力声明，`unknown` 保留现值。探测可能产生计费请求，但不会遍历整个模型目录，打开编辑器本身也不会自动发起探测。

ChatGPT Codex OAuth 后端使用 Responses 形状，但不接受公开 Responses API 的 `max_output_tokens` 和 `temperature` 参数。Ferry 在 Codex Provider 的出站边界定向移除这些已知不兼容字段，同时保留规范化请求中的原值用于调度、审计和调试；其他 Provider 的同名字段不受影响。已知兼容差异必须在对应 Provider 适配器中显式维护并由回归测试覆盖，不能全局删除，也不能继续把可预测的无效字段交给上游后再依赖 400 回退。

Codex 的 `response.metadata` 可携带 `openai_chatgpt_moderation_metadata` 诊断信息，它不表示生成内容或完成状态。Responses 原生流保留原始事件；非流式结果省略这项诊断并返回兼容提示，跨协议流也不把它当作文本。识别范围限于已验证的诊断结构；未知字段继续按不可无损转换处理，失败、拒绝和异常结束事件的检查不变。

Codex 工具续轮所需的 `encrypted reasoning` 是网关到上游之间的连续性状态，不是 Chat 客户端响应字段。Ferry 按内部会话隔离缓存并在下一次同会话工具结果前回放；Chat 客户端可见的 reasoning summary 映射为 `reasoning_content`。来自 Hermes、Codex、Claude 等客户端的会话证据会先解析为 Ferry 内部会话 ID，再派生稳定且账号隔离的 Codex session key，不能为每一轮随机生成。Anthropic 的 signed thinking 仍要求可回放的真实 signature，不能用 Responses 的无签名 reasoning 伪造。

工具历史还会在最终目标调用前做结构校验：定义名与 call ID 必须存在且唯一，参数/结果必须是合法 JSON，每组并行调用必须由紧随其后的对应结果完整闭合。畸形历史是 request-scoped `unsupported_parameter`，不会发送给任一上游，也不会错误触发熔断。

structured output 还会在响应提交前检查最终文本：`json_object` 必须得到一个完整 JSON object；带 schema 的请求至少必须得到合法 JSON。当前不在网关内重复实现完整 JSON Schema 验证，schema 约束仍由被标记为原生支持的上游负责；若返回值连合法 JSON 都不是，则按 target-scoped `bad_response` 回退，绝不把普通文本冒充结构化结果。

### Anthropic 结构化输出与厂商 schema 修正

Anthropic `output_config.format.type=json_schema` 与 Chat `response_format.json_schema`、Responses `text.format` 双向映射；OpenAI 的显式 `strict` 保留在 IR，Anthropic 原生 schema 转到 OpenAI 时发送 `strict:true`。显式 `strict:false`，以及 OpenAI 来源省略 `strict` 时的默认 false，转换到 Anthropic 原生 schema 会加强约束，因此记录兼容性诊断：默认兼容策略可透明尝试，`exact_only` 拒绝。Anthropic 来源和内部结构化输出探测使用 `strict:true`，不产生这项加强约束诊断。schema 原样传递，不删除不支持的约束，也不擅自补 `additionalProperties:false` 改变用户定义。上游拒绝仍作为失败处理，不能静默退成普通 JSON 或提示词。未知 `output_config` 字段／格式保留在原生透传路径，跨协议明确拒绝。无 schema 的 OpenAI `json_object` 没有相同原生映射，继续生成降级诊断并由现有转换策略控制：已通过能力筛选的目标在默认兼容策略下可尝试，`exact_only` 拒绝。

结构化输出编码能力与上游支持证据分开：模型声明、所选端点／连接的探测证据决定路由资格，缺少声明或证据仍是未知，明确负证据仍不支持。能力探测只证明该次有界请求的表现，不证明任意 schema 均受完整约束。现有提交前 JSON 检查继续生效；本批没有新增完整 JSON Schema 验证器，拒绝或截断也不承诺得到合规 JSON。

`ProviderAdapter` 是编码后的内置请求修正边界，见[适配器合同](provider-adapters.md)。Zhipu 原生 Chat 的字段校验通过这个接口执行；跨协议 Zhipu 字段映射继续留在 Chat 编码器。新增工具 schema 规则仅按实际解析的目标 URL 精确主机匹配，`FullURL` 优先于 `BaseURL`，不根据模型名或控制台品牌猜测：

| 规则 | 目标主机 | 变更及保留范围 |
| --- | --- | --- |
| Kimi enum type | `api.moonshot.ai`、`api.moonshot.cn`、`api.kimi.com`、`api.kimi.ai` | 仅对缺失 type 的非空、可确定为 string／integer／number 的 enum 补 type；混合字符串与数字、null／boolean／对象／数组、已有 type 和 const 不改 |
| DeepSeek NUL pattern | `api.deepseek.com` | 仅将工具 schema `pattern` 中独立的正则 `\0` 写为 `\u0000`；保留转义反斜杠、后接数字的转义、已有 Unicode 转义和其他字段 |

上述规则覆盖 Chat、Responses、Messages 的工具 schema，包括同协议与跨协议编码结果；只遍历 schema 关键字，不改 default／examples／enum 内的实例数据或请求正文。规范化器局部替换字节，不额外重排未修改字段；原有协议透传编码的行为保持不变。自定义代理地址不自动套用规则，即使其转发 Kimi 或 DeepSeek。

来源与验证边界（2026-10-24）：在线核对 [Anthropic 官方格式与限制](https://platform.claude.com/docs/en/build-with-claude/structured-outputs)、[OpenAI structured outputs](https://developers.openai.com/api/docs/guides/structured-outputs)、[Kimi Tool Use](https://platform.kimi.ai/docs/api/tool-use)、[官方 MFJS](https://github.com/MoonshotAI/walle/blob/main/docs/mfjs-spec.zh.md) 与 [DeepSeek Tool Calls](https://api-docs.deepseek.com/guides/tool_calls/)。MFJS 文档含无 type 的 enum 示例，所以缺失 type 的拒绝不是普遍官方规则；DeepSeek 官方也没有在该文档声明 `\0` 拒绝。两项修正的异常线索分别来自审计固定版本 Magpie [Kimi](https://github.com/yetone/magpie/blob/117937b8d9a08b0c85c507994899e5e025316d32/internal/gateway/kimi_schema.go)／[DeepSeek](https://github.com/yetone/magpie/blob/117937b8d9a08b0c85c507994899e5e025316d32/internal/gateway/deepseek_schema.go)，本项目使用独立实现及回归用例，没有引入第三方代码或依赖。通过本地 HTTP／SSE、能力证据和字节保留测试；未使用真实供应商账号验证，不将离线通过等同于供应商实测。

## 响应提交与恢复边界

- 非流响应在写入成功账本、保存会话或返回客户端之前检查 `ConversionIssues`。不可无损转换时记为 `protocol_conversion`，已产生的上游 usage 仍计入恢复开销，然后尝试下一路由。
- 流响应只有首个客户端可见事件才建立 commit。commit 前的空流、无终态、工具 ID/参数损坏或转换不兼容可安全回退。
- 同协议 SSE 的 `message_start`、`response.created`、ping、空 choices 等生命周期帧会先在内存中有界暂存；它们本身不会抢先建立 commit。收到首个有意义内容后按原顺序释放，若此前已经失败则全部丢弃并安全切换目标。暂存上限为 256 帧或 1 MiB，超限按断流处理，避免无内容上游无限占用内存。`[DONE]`、`message_stop`、`response.completed` 等原生终态帧则一直保留到对应规范化终态通过工具、finish reason 与 structured output 校验后才释放，避免先把成功终态写给客户端、再发现响应无效。
- commit 后绝不跨目标拼接两段流。后续断流或发现原生专属事件时，以协议内错误终止当前流，并把请求记为失败。
- 工具流必须满足 start → arguments* → end，ID、名称和最终参数 JSON 都必须完整；并行工具按事件 index 独立组装。
- 同协议原生响应只有在确有原始 body/SSE frame 可返回时才能绕过“未来扩展字段无法转换”的检查；已知协议终态或工具结构本身畸形时仍会失败关闭。仅标注相同协议但没有原始载荷也不能绕过检查。
