# Ferry 快速开始

先让你常用的模型通过 Ferry 回答一次，再把这个入口接到日常使用的 Agent 上。本指南从源码启动本地实例，带你完成上游接入和首个请求；随后可以继续配置[长任务恢复路由](long-tasks.md)。

示例默认只监听 `127.0.0.1`。容器安装见[运维指南](operations.md)。

## 1. 准备环境

- Go 1.26.10；下面的命令通过 `GOTOOLCHAIN` 固定该版本。
- 一个空的 `./data` 目录。若这里已有 Ferry 数据，请保留它并改用另一个目录，不要覆盖。
- 要验证真实推理，还需要你有权使用的上游账号或 API Key。上游可能计费。

在仓库根目录启动：

```bash
GOTOOLCHAIN=go1.26.10 go run ./cmd/ferry -data-dir ./data -config ./config.example.yaml
```

`config.example.yaml` 只在 SQLite 为空时导入一次。首次导入后，运行配置保存在 `./data/control.db`；后续启动可省略 `-config`：

```bash
GOTOOLCHAIN=go1.26.10 go run ./cmd/ferry -data-dir ./data
```

服务启动后可在另一个终端核对：

```bash
curl -fsS http://127.0.0.1:7890/health
curl -fsS http://127.0.0.1:7890/version
```

## 2. 初始化唯一管理员

打开 <http://127.0.0.1:7900>，创建唯一管理员。密码至少 12 个字符。

请在首次启动后立即完成这一步。未初始化的管理面允许能访问该地址的人认领管理员；示例配置只
监听本机，若你自行改为非 loopback 地址，先在网络层限制访问。

## 3. 接入你的第一个上游

在 **渠道（Providers）→ 添加渠道（Add Provider）** 中：

1. 选择 Provider 类型，填写上游 Base URL；
2. 填入你自己的 API Key，或完成支持的 OAuth 流程；
3. 选择实际上游模型和原生协议；
4. 复核后创建 Provider。

目录发现本身只读取模型列表；对尚无新鲜证据的已选择模型，能力验证可能发送最小的真实请求并
产生上游费用。Ferry 不会扫描或验证你没有选择的整个模型目录。

创建 Provider 时，Ferry 会为已配置的上游模型建立同名 Logical Model 和默认直连路由。
如果你另建 Logical Model，请先配置至少一条有效路由再启用它。

## 4. 创建推理 API Key

在 **API 密钥（API Keys）→ 创建 API 密钥（Create API Key）** 中创建客户端 Key。建议至少填写用途说明，并限制允许的模型、
协议、到期时间、RPM 或月预算。具有相应权限的管理员可以查看完整 Key；不要把它写入源码、日志、截图
或 Issue。

在终端中以隐藏输入保存 Key：

```bash
printf 'Ferry API key: '
read -r -s FERRY_KEY
printf '\n'
export FERRY_KEY
```

先确认 Key 能看到当前模型：

```bash
curl -fsS http://127.0.0.1:7890/v1/models \
  -H "Authorization: Bearer ${FERRY_KEY}"
```

然后将 `<logical-model>` 替换为上一步返回并已配置路由的模型：

```bash
curl -fsS http://127.0.0.1:7890/v1/chat/completions \
  -H "Authorization: Bearer ${FERRY_KEY}" \
  -H "Content-Type: application/json" \
  -d '{"model":"<logical-model>","messages":[{"role":"user","content":"Reply with: ready"}]}'
```

看到正常回复后，在 **请求（Requests）** 中打开这次调用，确认逻辑模型、实际线路和最终结果。若失败，从[请求排障](troubleshooting.md)核对 Key、路由、账号和能力证据。

## 5. 把入口交给你的 Agent

在客户端中设置 Ferry 的 Base URL、刚创建的 Key 和逻辑模型名。按客户端使用的协议选择 [Chat、Responses 或 Messages 入口](api.md)，然后运行一次你熟悉的任务。

接下来进入[长任务指南](long-tasks.md)：为同一模型名增加备用路由，设置恢复条件和首输出等待上限，再验证工具调用与续轮。只有一条正常线路时可以开始使用；添加经过验证的备用线路，才能在首选不可用时多一条路。

想让 Agent 帮忙检查配置或诊断请求，可以安装 [Ferry Skill](agent-skill.md)。

## 6. 停止与本地数据

前台进程使用 `Ctrl-C` 停止。运行数据位于 `./data`：

- `control.db` 保存管理员、配置 revision、推理 Key 和上游凭据；
- `conversation.db` 保存跨请求的会话绑定和续轮状态；
- `ledger.db` 保存请求、尝试、决策、用量和成本记录。

这三个数据库可能包含敏感数据。不要提交、上传、手工复制运行中的数据库，或把它们附在问题
报告中。备份、校验和恢复使用程序提供的 `-backup-to`、`-verify-backup` 和 `-restore-from`
入口；先用 `go run ./cmd/ferry -help` 查看参数。

## 参与开发时运行检查

参与开发时执行完整产品检查：

```bash
bash scripts/test.sh
```

完整检查还需要 Node.js、npm 和 Python 3。只验证 Go 代码时可执行：

```bash
GOTOOLCHAIN=go1.26.10 go test ./...
GOTOOLCHAIN=go1.26.10 go vet ./...
```
