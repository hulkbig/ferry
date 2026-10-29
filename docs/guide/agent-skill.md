# 让 Agent 帮你管理 Ferry

请求失败后，把请求 ID 交给 Agent，让它读记录、解释选路，再提出调整方案。Ferry 为这类工作提供正式的 Skill 和管理 API：Agent 与你在控制台中看到的是同一套模型、路由、配置版本和请求证据。

它使用专用 Management Token，无需主机 root 权限或管理员密码。你决定可授予的权限，高风险操作仍由服务端开关控制。

## 从一项具体工作开始

安装后，可以让管理 Agent 执行这样的任务：

- “盘点当前模型和可用线路，列出需要处理的连接状态。”
- “查看这个请求为什么等待、为什么换路，以及最终用了哪个上游。”
- “为这个模型准备备用路由，模拟选路结果，展示配置差异。”

配置变更遵循草稿、校验／模拟、审阅和 revision 检查；应用后回读结果。授权范围内可以继续执行，超出范围的变更先交给你确认。完整流程见[操作工作流](../../skills/ferry/references/workflows.md)。

## 安装

Ferry 随程序分发管理 Skill、Python 客户端和接口参考。安装包来自正在使用的实例，无需 npm 或 pip 依赖；运行客户端需要 Python 3。

在控制台 **Settings → Security → Install Ferry Skill** 复制安装说明。让助手按该实例的 `INSTALL.md` 下载同目录 `ferry.zip` 和 `ferry.zip.sha256`，校验后安装完整 `ferry/` 目录。只保存 `SKILL.md` 不足以运行其中的脚本。

源码中的[安装说明](../../skills/ferry/INSTALL.md)与[Skill 入口](../../skills/ferry/SKILL.md)说明了包内容和安装检查。ZIP 及 checksum 由运行中的服务提供，不是源码中预放的文件。

## 配置身份

在控制台创建专用 Management Token，通过自己的秘密管理器或进程环境提供：

- `FERRY_MANAGEMENT_URL`：管理端 origin，不加 `/api`。
- `FERRY_MANAGEMENT_TOKEN`：该 Token 的值。
- `FERRY_DATA_URL` 与 `FERRY_API_KEY`：仅在需要推理时配置。

安装不会自动创建 Token、授予高风险权限或调用模型。不要把凭据放进安装提示、URL、截图或聊天。

## 检查接入

用已安装客户端读取访问权限：

```text
python3 <installed-skill>/scripts/ferry.py management GET /api/access
```

成功后核对 Token 身份及 `allow_high_risk`。只读盘点不需要开启高风险权限；配置应用、密钥生命周期和 OAuth 写入等操作要求独立授权与服务端权限开关。失败时检查地址和身份，不借用其他用户的 Token。

完整资源和请求格式见[管理 API](../../skills/ferry/references/management-api.md)。
