# 安全问题

[English](SECURITY.en.md)

## 报告渠道

本仓库已启用 GitHub 私密漏洞报告。

请在 `hulkbig/ferry` 的 **Security → Report a vulnerability** 提交：[私密报告入口](https://github.com/hulkbig/ferry/security/advisories/new)。入口不可用时，使用已有的维护者私密联系方式，或先请求私密联系渠道，不披露漏洞细节。不要在公开 Issue 或 PR 中提交利用步骤。

不要上传数据库、备份、令牌、完整 OAuth 回调地址或用户正文。普通功能问题使用 [Issues](https://github.com/hulkbig/ferry/issues)，见[贡献说明](CONTRIBUTING.md)。

## 有用的信息

报告中注明受影响版本、运行方式、影响范围、最小复现、必要的脱敏日志，以及是否已发生实际数据暴露。可以先提供问题概述，再通过私密渠道交换复现材料。不要测试未经授权的实例。

## 支持版本与响应

当前支持 `1.x` 系列的最新稳定版本，现为 `1.0.0`。`0.1.x` 使用者应升级；1.0.0 已包含 Codex 用量刷新与额度保护修复。较早的开发快照没有独立支持承诺。

项目不承诺固定的响应或修复时限。维护者先确认影响并复现，再安排修复与披露。候选构建或 CI 通过不代表已完成安全审计。

## 部署边界

- 在受控环境完成首次管理员初始化，再开放访问。
- 管理端远程访问需要 TLS 和访问限制，不建议直接暴露公网。
- 使用独立且受限的客户端 Key 与 Management Token。
- 保护 SQLite、WAL、备份和请求日志；默认不提供静态加密。
- 调试正文可能包含敏感数据，按需短期开启并检查保留策略。

完整说明见[安全模型](docs/security-threat-model.md)、[运维指南](docs/guide/operations.md)和 [Docker 安装与运维](docs/guide/docker.md)。
