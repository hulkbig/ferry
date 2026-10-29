# Ferry 介绍文案

以下为本地内容草稿。正式发布时附上实际可用的源码、Docker Hub、版本说明和快速开始链接；未发布前不使用“现已开放下载”或“复制即可安装”等表述。

## 一句话

**Ferry 渡口：少守着重试，让 AI 继续干活。**

为 AI Agent 长任务设计的自托管网关。限流时等待，故障时接力，高峰时排队；客户端使用一个稳定入口，AI 通过 Skill 和管理 API 参与运维。

## 短介绍

任务交给 Agent，过一会儿回来，却停在一次限流或一条坏线路上。

Ferry 把上游选路、等待和恢复放到一个自托管网关里。在恢复条件与预算允许时，限流后自动续接，线路失败后让合适的备用上游接手，并用并发控制和队列处理请求高峰。客户端保留熟悉的模型入口，AI 也能通过 Skill 检查请求、模拟路由和执行授权变更。

你掌握上游账号与运行数据，Ferry 帮你减少照看任务的次数。

## 八条优势

1. **限流等待，自动续接**：遇到临时限流，在恢复窗口内等待，条件允许后继续响应。
2. **上游故障，自动接力**：线路出现可恢复故障，由符合任务要求的备用线路接手。
3. **并发可控，高峰排队**：设置并发上限，超出容量按策略等待，减少请求突发。
4. **AI 友好，自助运维**：用 Skill 和管理 API 读证据、准备草稿、模拟并执行授权变更。
5. **能力实测，按需匹配**：基于具体模型与协议的能力证据，选择能承接请求的线路。
6. **入口不变，模型可换**：上游变化时，客户端继续使用同一个逻辑模型名。
7. **三种协议，统一接入**：Chat、Responses、Messages 都有明确入口与能力边界。
8. **请求过程，有据可查**：从一次请求看到实际尝试、恢复原因、用量与费用依据。

## English introduction

**Ferry — Less time retrying. More time for your AI to work.**

A self-hosted gateway for long-running AI agent tasks. Ferry waits through eligible rate limits, switches failed upstreams and queues traffic at capacity, all within configured recovery policies and budgets. Clients keep one stable model endpoint. Agents can use a bundled Skill and management API to inspect evidence, simulate routing and make authorized changes.

One Go process, an embedded console and local SQLite. Bring your own upstream accounts and keep control of your runtime data.

## 配图搭配

- 短介绍：现有抽象请求流主视觉，保留 Logo 与主张，避免密集叠字。
- 解释用途：新增[接入关系图](../assets/gateway-overview-zh.svg)。
- 解释连续性：[限流、故障、排队图](../assets/continuity-zh.svg)。
- 解释 AI 友好：[AI 管理流程图](../assets/agent-native-zh.svg)。
- 证明操作体验：后续补真实控制台示例截图，分别展示恢复请求详情和路由配置。

图注和正文都不把示意图写成实测结果，不把客户端 Thinking 写成模型无限推理，不把并发控制写成防封承诺。正式文案需要保留可查阅的能力与验证范围。
