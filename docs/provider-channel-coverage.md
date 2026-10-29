# 渠道支持与限制

Ferry 中的接入方式分为 API Key、Codex OAuth、原生账号和外部兼容网关。预设只是配置起点；真实可用性由账号权限、端点、模型、协议和请求能力共同决定。

## API Key 与兼容端点

控制台预设提供名称、Base URL、起始探测协议及文本转换设置，不包含凭据，也不会按模型名称补出能力或额度。可以编辑预设，或使用 **Custom endpoint** 接入其他兼容服务。

| 类别 | 控制台中的代表入口 |
| --- | --- |
| 供应商 API | OpenAI、Anthropic、Google Gemini、DeepSeek、Moonshot、Zhipu、Alibaba Model Studio、Volcengine Ark、Mistral、xAI、Perplexity |
| 其他兼容 API | Groq、Together、Fireworks、Cerebras、MiniMax、Baidu Qianfan、Tencent Hunyuan、NVIDIA、Hugging Face |
| 聚合与网关 | OpenRouter、SiliconFlow、New API／One API、自建 CLIProxyAPI |
| 订阅入口 | GLM、Z.ai、Alibaba、Ark、Kimi 等专用 Coding／Token 方案；按套餐和区域选择配置 |
| 自定义 | 由部署者填写可达地址、凭据和支持的协议 |

完整预设以[控制台定义](../web/console/app.part9.jsfrag)为准。同品牌的中国／国际或订阅／按量入口可能使用不同地址与凭据，不能混用。目录发现只读取模型；对明确选定的模型执行能力探测时可能产生费用。

三种文本入口的转换开关不表示上游原生支持三种协议。图片只走原生 Images 路径。无法表达的参数按[协议行为](protocol-fidelity.md)处理。

Anthropic Messages 的 `output_config.format` 结构化输出已有双向映射与能力探测；支持与否按具体模型、端点／连接的声明和证据判断，不对所有 Anthropic 兼容渠道统一开启。仅 `json_object` 的请求仍存在显式提示词降级边界。

Kimi／Moonshot 官方主机的工具 enum 类型补全，以及 DeepSeek 官方主机的工具 NUL 正则转义修正，已接入内部 `ProviderAdapter`。规则不按模型名套用，也不自动扩展到自建代理；`FullURL` 决定最终主机。Zhipu 原生 Chat 校验已迁入同一边界。详细主机、保留语义与证据见[协议行为](protocol-fidelity.md)，接口职责见[适配器合同](provider-adapters.md)。本批为独立离线测试覆盖，尚无真实供应商账号验证，不代表全部模型／schema 均受支持。

## Codex OAuth

Codex 使用独立登录、刷新、配额和重置卡流程。账号面板显示本地重置请求与上游卡片历史；自动／手动来源和执行结果分开记录，无法判断的外部使用来源显示为未知。自动使用重置卡可在设置中关闭或调整阈值。

账号可见模型、Fast 声明与用量以实际目录和账号快照为依据，不能由套餐名称推断。参见[模型同步](provider-model-sync.md)、[订阅用量](provider-subscription-usage.md)和[服务等级证据](service-tier-evidence.md)。

## 原生账号驱动

以下驱动已接入登录、凭据生命周期、管理 API 和运行链路。**目前通过的是离线测试，尚未逐一验证真实供应商账号准入与推理。** 不要将此表作为供应商可用性或服务承诺。

| 驱动 ID | 登录与调用路径 | 当前限制 |
| --- | --- | --- |
| `claude` | 浏览器 PKCE、刷新、Anthropic Messages | 需账号准入及实际模型能力；不因登录成功推断权限 |
| `kimi` / `kimi-ai` | 国内／国际设备授权、刷新、Chat | 区域凭据隔离；按实际目录和模型配置 |
| `xai` | 设备授权、OIDC发现、刷新、Responses | 直接 API 路径，不承诺 CLI 专属准入 |
| `meta` | 设备授权、换取推理 Key、Responses | `key_pending` 需显式完成换 Key；不承诺媒体能力 |
| `vertex` | 服务账号、JWT换令牌、项目／区域绑定的 OpenAPI Chat | 手工模型 ID；不含 ADC 或全部 Gemini 原生协议 |
| `antigravity` | 操作者的 OAuth client、PKCE、项目发现、Cloud Code 适配 | 手工模型 ID；文本／函数工具；拒绝未实现的媒体和签名状态 |
| `devin` | PKCE、会话、Connect/protobuf适配 | 手工模型 UID；文本／工具；会话失效需重新登录 |

这是 7 个家族、8 个驱动 ID，Codex 不计入其中。控制台从 `/api/oauth/native/drivers` 读取实际注册项。

登录只改变账号凭据；Provider 保存是另一个配置操作。运行配置使用 `native-oauth:<driver>:<account>` 引用。刷新结果不确定时可能进入 `relogin_required`，不盲目重放已轮换的刷新令牌。Vertex、Antigravity、Devin 默认关闭模型目录同步。

字段、流程和测试范围见[原生驱动参考](reference/native/README.md)。

## 外部网关

外部 CLIProxyAPI 或其他兼容网关可以作为普通上游，使用该网关的客户端推理 Key。登录、账号配额和凭据续期由实际上游服务负责，不能把它的能力计为 Ferry 原生实现。

填写从 Ferry 所在主机／容器可达的地址。容器内 `localhost` 通常不是外部网关所在位置。先验证上游，再配置 Ferry 路由；停用该网关前需要迁移供应商接入。

## 尚未覆盖的路径

- AI Studio 浏览器中继、完整 Gemini Interactions 和全部 Gemini 专属语义。
- AWS SigV4／STS 生命周期、Bedrock 原生 Converse／Invoke 适配。
- Azure Entra 凭据生命周期、资源发现，以及所有旧版 deployment API 变体。
- 通用多租户账号隔离和多个实例共享 SQLite。

具有兼容 API 的具体端点仍可使用 Custom 配置并单独验证；这不等于上述原生功能已实现。
