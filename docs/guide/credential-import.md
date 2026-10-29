# 从 sub2api / CLIProxyAPI 导入账号与 Key

[English](credential-import.en.md)

此功能读取管理员主动上传的文件，将支持的凭证写入 Ferry 私有凭证存储，并创建禁用的渠道或客户端 API Key。不会修改来源服务，不会自动接管流量，也不会读取服务器上的任意路径或连接来源管理 API。

## 能力盘点

| 来源内容 | 当前处理 |
| --- | --- |
| sub2api `sub2api-data` / `sub2api-bundle` v1 账号导出（含 `data` 包装） | 逐账号解析 `accounts[].credentials` |
| sub2api OpenAI OAuth | 导入 Codex；映射 `chatgpt_account_id`、`expires_at`，要求 access/refresh token |
| sub2api Anthropic OAuth | 导入 Claude 原生 OAuth，要求 access/refresh token |
| sub2api OpenAI / Anthropic / xAI `apikey` | 导入上游 Key 与端点 |
| CLIProxyAPI 旧版 `openai-compatibility` / `api-key-entries`、`claude-api-key`、`codex-api-key` | 导入上游 Key、端点和显式模型名称；Codex Key 要求显式端点 |
| CLIProxyAPI v8 `api-keys.<provider>[].keys[]` | 支持 openai-compatibility、claude、codex、xai 分组；每个 Key 独立成为渠道 |
| CLIProxyAPI 单个或多个 Codex / Claude auth JSON | 原始文件上传；列表接口的元数据不是凭证文件 |
| CLIProxyAPI 旧版顶层 `api-keys: []`、v8 `access.api-keys` | 导入原始客户端 Key，保持禁用 |
| sub2api 客户端 Key | 账号导出不包含这些 Key；通过补充 JSON 数组显式提供 |
| Gemini / Vertex 原生 Key、Antigravity / Gemini / 其他 OAuth、Codex SIWC、不同 OAuth client | 明确标为不支持，不保存该项凭证 |
| 代理、路由、模型别名、排除列表、附加头、重试、组权限、预算/余额、过期策略 | 不迁移；预览提示人工核对，不将其误映射为 Ferry 权限 |

格式依据：2026-10-27 核对 [sub2api account_data.go](https://github.com/Wei-Shaw/sub2api/blob/ba81c7fd758e425d67f856d601147ef0c5263d3f/backend/internal/handler/admin/account_data.go) 与 [CLIProxyAPI config.example.yaml](https://github.com/router-for-me/CLIProxyAPI/blob/1f917e77c4562623aa49e9804d999524151a2f96/config.example.yaml)。这是格式兼容范围，不代表对真实账号的可调用性认证。

端点转换保留来源的请求路径语义：Claude API Key 的根地址补入 `/v1`，sub2api OpenAI 的无版本地址补入 `/v1`，已有版本路径保留。CLIProxyAPI 第三方 Claude 网关使用 Bearer，官方 Anthropic 使用 `x-api-key`；sub2api 的显式 Anthropic 认证方案也会保留。预览显示转换后的端点与认证头类型，具体兼容性仍需启用前核对。

## 控制台操作

1. 登录 Ferry，进入 **Settings → Import & Migration（设置 → 导入与迁移）**。渠道页的“导入账号”入口也会进入此页签。
2. 选择来源和来源标识，上传账号导出/配置文件；可另外选择多个 OAuth JSON 文件和客户端 Key 文件。
3. 点击“解析并预览”。预览不写凭证、不调用上游，返回脱敏清单、不支持项和迁移限制。
4. 勾选要导入的条目，点击“导入选中项（保持禁用）”，逐项查看结果。
5. 回到控制台审核端点、配置代理、发现并验证模型、配置客户端权限/额度/过期时间，确认后启用渠道和连接。导入不会生成可调用性或完整能力证据，也不自动创建业务路由。

补充客户端 Key 文件格式：

```json
[{"name":"my-client","key":"REPLACE_WITH_SOURCE_CLIENT_KEY"}]
```

旧的 `/import.html` 书签会自动跳转到 `/#/settings/import`，沿用控制台登录态、主题与语言。结果清单提供渠道和客户端 Key 的核对入口。

单次最多 500 项、文件内容合计 4 MiB。客户端身份使用稳定的导入 ID 命名，来源展示名保留在预览中。源文件只保留在页面内存中，不写浏览器持久存储；处理完成后清空上传选择。离开导入页签、退出登录或关闭页面也会释放请求与文件引用。

## 重复、失败与凭证刷新

来源标识、来源类型和条目位置决定稳定对象 ID。**同一份导出重试必须保持来源标识和条目顺序不变**；已有同 ID 对象只报告 `exists`，不会替换凭证或配置。换标识或重排文件不是更新机制，可能产生新对象。重复客户端 Key 还会对现有 ACTIVE/GRACE 凭证做哈希查重，避免另建身份改变现有访问行为。

每项独立保存，批次不是一个跨存储事务；部分失败返回 HTTP 207，成功项不回滚。先保存凭证再通过现有配置发布管线创建禁用对象。发布失败时，已保存的凭证保留，响应提供 `credential_ref`，不会删除可能已被其他操作引用的记录；重试使用新的凭证名称，不覆盖旧记录。未绑定凭证需管理员核对后清理。客户端 Key 通过现有密封存储持久化，配置修订不包含原始 Key。

请求结果未知时，重新预览并核对已有对象后再试。UI 不自动重放写入。重新导入不是 OAuth 凭证更新或轮换入口。

迁入的 Codex / Claude 凭证带持久化等待标记，后台刷新不会主动轮换它们，重启后仍然有效。首次显式探测/使用所需的按需刷新成功后进入正常刷新周期。启用前协调来源实例停止使用同一份可轮换 refresh token；本功能不会停止来源实例。未知过期时间按已过期处理，首次使用先刷新，不猜测有效期。

## 管理 API

两个接口均走管理认证和 CSRF；Management Token 需要管理员授予 `allow_high_risk`。响应设置 `Cache-Control: no-store`，不返回 access token、refresh token 或 API Key。

- `POST /api/imports/preview`：解析、不写入。
- `POST /api/imports/apply`：重新解析请求，导入 `selected` 明确列出的条目。

```json
{
  "source":"sub2api",
  "namespace":"office",
  "document":"{\"version\":1,\"accounts\":[]}",
  "auth_files":[],
  "client_keys":[{"name":"my-client","key":"REPLACE_WITH_SOURCE_CLIENT_KEY"}],
  "selected":[]
}
```

预览返回 `items`（`id/name/kind/status/warnings/error`，可包含端点、协议、模型）及全局 `warnings`。应用时回传相同输入，并把预览选中的 `id` 放进 `selected`。状态为 `ready`、`unsupported`、`exists`、`imported`、`failed`；应用成功仅表示已禁用保存，不代表上游验证成功。不支持项可与成功项同时出现在结果里。

本地验证覆盖：新旧源格式、凭证脱敏、异常/掩码凭证、端点限制、选择校验、权限门禁、重复与并发导入、客户端禁用及密封保存、失败后回读/重试、OAuth 等待状态跨重启、UI 文件预览/提交与未知结果处理。真实来源实例导出、真实 OAuth 刷新及上游推理需要使用者另行验证。
