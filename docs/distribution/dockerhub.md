# Docker Hub 介绍草稿

维护说明：这是发布前的内容源，尚未同步到 Docker Hub。仓库为 `bighulk/ferry`；已准备固定版本安装命令；公共 CI、公开图片地址与匿名拉取仍待确认。以下内容不表示镜像已公开。

## Short description

Self-hosted AI gateway: rate-limit recovery, upstream failover, concurrency control and agent operations.

中文短介绍：为 AI 长任务设计的自托管网关：限流续接、故障接力、并发排队，支持 AI 自助运维。

## Overview — English

### Ferry · Less time retrying. More time for your AI to work.

You hand work to an AI agent, then come back to a rate limit or a failed connection. Ferry gives that agent a stable model endpoint and handles routing, waiting and recovery under your policies.

Ferry runs as one Go process with an embedded web console and local SQLite storage. You bring your own upstream API keys or supported subscription accounts. Node.js and an external database are not required at runtime.

![How clients, Ferry, upstreams and agent operations connect](https://raw.githubusercontent.com/hulkbig/ferry/v0.1.0/docs/assets/gateway-overview-en.svg)

### Install

```sh
curl -fsSLo compose.yaml https://github.com/hulkbig/ferry/releases/download/v0.1.0/compose.yaml
docker compose up -d
```

Open **http://localhost:17910** and create the administrator. Image: `bighulk/ferry:0.1.0`, Linux AMD64 / ARM64. No source checkout or separate database is needed.

[Installation, remote access, backups and upgrades](https://github.com/hulkbig/ferry/blob/v0.1.0/docs/guide/docker.en.md) · [中文安装指南](https://github.com/hulkbig/ferry/blob/v0.1.0/docs/guide/docker.md) · [Source and issues](https://github.com/hulkbig/ferry)

### What Ferry handles

- **Wait through rate limits.** Keep eligible requests pending within the recovery window and resume when conditions allow.
- **Switch failed upstreams.** Let an eligible fallback take over after a recoverable connection failure, transient error or configured first-output timeout.
- **Control concurrency.** Limit active requests and queue excess traffic according to policy.
- **Let agents help operate it.** Use the bundled Skill and management API to inspect requests, simulate routing and make authorized changes.
- **Match tested capabilities.** Route using evidence for the selected model, endpoint and protocol; apply registered compatibility mappings where allowed.
- **Keep one model endpoint.** Change upstream models and routes while clients retain the same logical model name.
- **Use familiar protocols.** Connect through OpenAI Chat, Responses or Anthropic Messages, subject to each route's capabilities.
- **Explain a request.** Inspect upstream attempts, recovery decisions, usage and cost records together.

Recovery has time and attempt budgets and honors client cancellation. Ferry does not splice a new upstream stream into output already committed to the client. A client's “Thinking” indicator is not a promise of continuous upstream reasoning.

### Container essentials

| Item | Value |
| --- | --- |
| Management port inside the container | `7900` |
| Inference port inside the container | `7890` |
| Persistent data | `/data` volume; databases in `/data/state`, on local storage |
| Runtime user | UID / GID `10001:10001` |
| Initial setup | Open the console, create the administrator, add an upstream and create a client API key |
| Health endpoint | `/health` on the inference port; health alone does not verify an upstream |

The public installation block must include a version-pinned image, loopback-bound host ports, a persistent volume and a restart policy. Complete administrator initialization before enabling remote access. Do not run multiple instances against the same SQLite data directory.

### Upgrades and support

Back up and verify data before upgrading. Retain the previous image and a tested recovery path; changing an image back does not undo a database migration. Report the exact version, architecture and a minimal redacted reproduction when requesting help.

Ferry's original code is licensed under Apache-2.0. Third-party notices are included with the image. Provider subscriptions and credits are not included.

## Overview — 中文

### Ferry 渡口 · 少守着重试，让 AI 继续干活。

把任务交给 Agent 后，你希望回来看到进展。Ferry 在客户端与上游之间提供一个稳定入口，按你设置的策略处理等待、选路和恢复，减少遇到限流或坏线路时的手动重试。

一个 Go 进程，内置控制台，使用本地 SQLite。运行时无需 Node.js 或外部数据库；上游账号和额度由你提供，配置与请求数据由你保管。

![客户端、Ferry、上游与 AI 运维入口的关系](https://raw.githubusercontent.com/hulkbig/ferry/v0.1.0/docs/assets/gateway-overview-zh.svg)

| 核心能力 | 为你做什么 |
| --- | --- |
| 限流等待，自动续接 | 恢复窗口内等待，条件允许后继续响应 |
| 上游故障，自动接力 | 可恢复故障发生后，按策略选择合适备用线路 |
| 并发可控，高峰排队 | 控制请求节奏，容量不足时按策略排队 |
| AI 友好，自助运维 | 通过 Skill 与管理 API 检查证据、模拟方案、执行授权变更 |
| 能力实测，按需匹配 | 根据选定端点、模型与协议的能力证据选路 |
| 入口不变，模型可换 | 客户端保持逻辑模型名，上游线路由 Ferry 安排 |
| 三种协议，统一接入 | 支持 Chat、Responses、Messages 入口及对应能力范围 |
| 请求过程，有据可查 | 查看尝试、恢复原因、用量和费用依据 |

恢复遵守时间与次数预算，并响应客户端取消；已经提交可见输出的流不会被另一条流拼接替换。并发控制用于管理请求节奏，不保证账号不会被封禁。

容器内管理端口为 `7900`，推理端口为 `7890`，数据卷挂载至 `/data`。首次进入控制台创建管理员，添加上游，再创建客户端 Key。远程访问前完成初始化并配置 TLS 和访问限制。升级前先验证备份与恢复路径。

## 同步前需要替换的内容

1. 在 Overview 顶部加入实际验收过的 `docker run` 和 Compose 安装块，写明浏览器地址及服务器远程访问步骤。
2. 填写准确版本、镜像摘要、已验证架构、Release 与支持入口；删除草稿提示和本节。
3. 将图片和文档引用转换为公开可访问的绝对 HTTPS 链接，并以未登录身份验证。不能指向私有仓库或内网服务。
4. 加入标注“示例数据”的真实产品截图；在 Docker Hub 页面实际检查图片、代码块、表格与移动端显示。
5. 同步短介绍、源码地址、许可证与文档链接；首次公开前核对匿名拉取，随后按版本更新。
