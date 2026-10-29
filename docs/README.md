# Ferry 文档

[English](README.en.md)

从第一条请求，到一条能恢复、能解释、能交给 Agent 管理的路由。按你现在要做的事，选一个入口。

## 开始把任务交出去

| 你想做什么 | 从这里开始 |
| --- | --- |
| 在自己的机器上跑起来 | [快速开始](guide/quickstart.md)：启动、接入上游、发出第一个请求 |
| 让长任务少被临时故障打断 | [长任务指南](guide/long-tasks.md)：稳定模型名、备用路由、恢复条件与实际验证 |
| 更换模型，不改客户端配置 | [逻辑模型](guide/logical-models.md)：业务模型名与上游线路解耦 |
| 在 Hermes 使用已有订阅 | [Hermes Agent](guide/hermes.md)：账号、线路和客户端配置 |
| 自动兑换快过期的重置券 | [AutoReset](guide/autoreset.md)：阈值、跳过条件与结果记录 |
| 接入已有客户端 | [API 接入](guide/api.md)：Chat、Responses、Messages、流式与图片 |
| 让 Agent 帮忙管理 | [Agent Skill](guide/agent-skill.md)：安装、专用身份、诊断与受控变更 |
| 导入第三方账号与 Key | [导入与迁移](guide/credential-import.md)：sub2api / CLIProxyAPI 文件预览、禁用导入及审核 |
| 从旧实例迁入 | [迁移说明](guide/migration.md)：SmartAPI 数据、客户端和安装路径 |

## 常见问题与参与

[常见问题](guide/faq.md) · [贡献与 PR 步骤](../CONTRIBUTING.md) · [安全报告](../SECURITY.md)。这些文档、开发指南和排障指南均提供中英文版本；Issue 表单与 PR 模板可任选一种语言填写。

## 把这条路用好

- **选路与账号**：[配置说明](guide/configuration.md)解释模型、连接、路由和身份；[渠道支持](provider-channel-coverage.md)列出接入方式与验证范围。
- **请求卡住或失败**：[排障指南](guide/troubleshooting.md)从请求记录定位问题；[协议行为](protocol-fidelity.md)说明工具、推理与转换边界。
- **日常运行**：[运维指南](guide/operations.md)覆盖容器、日志、备份恢复和升级；[订阅用量](provider-subscription-usage.md)说明配额与重置来源。

## 查一个准确的行为

| 参考 | 解答的问题 |
| --- | --- |
| [模型同步](provider-model-sync.md) | 模型目录如何更新，能力证据如何使用？ |
| [协议行为](protocol-fidelity.md) | 哪些字段原样保留，哪些可以转换，哪些会被拒绝？ |
| [渠道适配边界](provider-adapters.md) | 如何新增内置渠道，认证、协议和厂商规则分别由谁负责？ |
| [Fast 与服务等级证据](service-tier-evidence.md) | 请求了什么档位，上游回显了什么，能据此判断什么？ |
| [原生驱动](reference/native/README.md) | 各渠道怎样登录、刷新，当前有什么限制？ |
| [管理 API](../skills/ferry/references/management-api.md) | 自动化如何读取资源、准备草稿和应用变更？ |

## 一起改进 Ferry

复现一次工具续轮失败、补充一个上游的能力证据，或把安装中绕远的一步写清楚，都能让下一位使用者少一次中断。参与方式见[贡献说明](../CONTRIBUTING.md)，代码结构与检查命令见[开发指南](development.md)。

- 项目表达：[品牌与文案](brand.md)、[对外发布与展示清单](distribution/README.md)、[介绍文案](distribution/announcement.md)。
- 构建与发布：[本地候选构建](local-candidate-builds.md)、[发布检查](release-readiness.md)。
- 安全与来源：[安全报告](../SECURITY.md)、[安全模型](security-threat-model.md)、[依赖声明](third-party-notices.md)、[容器材料](container-notices.md)、[资产来源](asset-provenance.md)。

当前为源码候选版。安装、FAQ、排障、贡献、开发和安全报告提供中英文版本；其余详细指南的语言见 [English 文档索引](README.en.md)。[旧控制台设计](archive/console-v1.md)保留为历史资料，当前接口以管理 API 文档和实现为准。

## Docker 发行安装

- [中文：安装、远程访问、备份与升级](guide/docker.md)
- [English: installation, backups and upgrades](guide/docker.en.md)

## 官网

[官网源码与本地预览](../website/README.md) · [公开官网](https://ferry.bighulk.dev/)（首次发行后开放）。

[Codex 额度保护](guide/codex-usage-protection.md)：默认 98% 暂停，付费 Credits 默认关闭；优先复用响应中的额度信息。
