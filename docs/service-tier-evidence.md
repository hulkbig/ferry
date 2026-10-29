# Fast 请求与服务等级证据

Fast 能力、请求参数和实际执行档位是三个独立事实。Ferry 的路由仍要求账户目录或
端点契约明确声明 `capabilities.priority`；本功能不增加能力标记，不按模型名称或耗时
推断支持，`ultrafast` 继续拒绝。

## 请求记录

`/api/requests` 列表、详情中的 `client_request` 和 `attempts` 保留：

| 字段 | 含义 |
| --- | --- |
| `service_tier` | 客户端请求值，入口去空格并转为小写 |
| `response_model`、`response_service_tier` | 实际上游返回的原始模型与档位，不用请求值填充 |
| `service_tier_evidence.wire_service_tier` | 最终序列化并交给 HTTP 客户端的档位，Codex 将 `fast` 转为 `priority`；未发送档位时省略 |
| `service_tier_evidence.response_tier_authority` | `authoritative`、`informational` 或 `unknown` |
| `service_tier_evidence.source` | `openai_api`、`codex`、`unknown` 或 `redirect` |
| `service_tier_evidence.priority_confirmation` | `confirmed`、`downgraded`、`unconfirmed` 或 `not_requested` |

只有公共 OpenAI HTTPS `/v1/responses` 或 `/v1/chat/completions` 的精确端点契约被识别为
`authoritative`。Codex OAuth 适配器和规范 ChatGPT Codex Responses 地址为
`informational`。其他网关、未知端点或发生 HTTP 重定向的请求均为 `unknown`。
能力声明不改变回显的可信程度。

当请求为 `priority` / `fast`，实际发送 `priority`，请求成功完成且档位来源可信时，
完成响应的 `priority` 表示 `confirmed`，`default` / `flex` 表示 `downgraded`。
Codex 的 `default` 和 `priority` 回显均只记录为 `unconfirmed`：不能据此证明加速或降级。
缺失档位、仅收到初始 Responses 事件、失败和取消均不能确认；未请求 Fast 时为 `not_requested`。

每次上游尝试使用独立观测记录，失败重试不会污染最终响应。请求级记录来自最后一次尝试；
详情保留所有尝试。观测表示 HTTP 发送尝试，不保证网络失败时上游收到请求。
历史记录和发送前拒绝的请求没有观测，字段为 `null` / 缺失，不从当前路由反推历史事实。
数据库采用可重复的增量列迁移，不改写历史请求。

## 数据面元数据

成功交付的 OpenAI Chat / Responses 响应增加两个响应头，流式响应在首次写入前设置：

```text
X-Ferry-Response-Tier-Authority: informational
X-Ferry-Response-Tier-Source: codex
```

响应头来自实际提交给客户端的那次尝试，只描述回显契约。它们不宣告流式请求最终成功、
不改变上游响应体、不把 `default` 改写成 `priority`。没有观测时省略响应头；客户端应按未知处理。
最终确认结果以完成后的请求记录为准。

`GET /v1/models` 每个模型增加独立于 `supported_service_tiers` 的字段：

```json
{
  "response_tier_authority": {
    "openai-chat": "informational",
    "openai-responses": "informational"
  }
}
```

目录检查默认路由、后备目标和可能生效的条件路由，尊重启用状态和协议转换限制。
所有可达目标的回显契约一致时发布对应值；混合、未知或没有可达目标时发布 `unknown`。
这是配置快照，不是一次实际执行的结果，路由变化后需要重新同步客户端。

## OpenCodeX 对接

OpenCodeX 支持 `responseTierAuthoritative` 后，可用 Skill 的
`sync_opencodex_fast.py --provider ferry --sync-tier-authority` 预览；加 `--apply` 保存并刷新。
此选项默认关闭，兼容不支持新字段的旧版 OpenCodeX。

客户端当前字段作用于整个 Provider：只有所用模型/协议的目录值全部为 `authoritative`
才同步 `true`；Codex、混合或未知目录同步 `false`。此值不会开启 Fast，逐模型能力仍独立同步。
脚本先保存并回读新字段，再写能力声明；旧客户端拒绝或丢弃字段时停止，不能声称已对接。
旧服务器没有此元数据时也停止写入。

当前 OpenCodeX 配置修复不自动消费上述响应头。混合网关设置为 `false` 会放弃对部分
公共 API 响应的客户端确认，以避免把 Codex 回显误判为降级；Ferry 台账仍按每次实际
请求区分。后续客户端逐响应读取响应头后，才可更精细地处理混合路由。
