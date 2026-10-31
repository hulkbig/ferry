# Ferry 快速开始

使用已发布的 Docker 镜像启动 Ferry，接入自己的上游，再完成第一个请求。无需下载源码、安装 Go 或本地编译。

## 1. 拉取镜像并启动

需要 Docker Engine 和 Compose。Mac 上已有 OrbStack 或 Docker Desktop 即可。在空目录执行：

```sh
curl -fsSLo compose.yaml https://github.com/hulkbig/ferry/releases/latest/download/compose.yaml
docker compose up -d --no-build
```

Compose 会自动拉取对应架构的 `bighulk/ferry:1.1.0` 镜像。配置和记录保存在命名卷 `ferry-data` 中。已有安装请按[升级指南](docker.md#升级)操作；不要把旧数据目录当成空目录重新初始化。

```sh
curl -fsS http://localhost:17810/health
curl -fsS http://localhost:17810/version
```

服务器安装、单条 `docker run` 命令和端口配置见[Docker 安装](docker.md)。源码构建仅用于[参与开发](../development.md)。

## 2. 初始化唯一管理员

打开 <http://localhost:17910>，创建唯一管理员。密码至少 12 个字符。

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
curl -fsS http://localhost:17810/v1/models \
  -H "Authorization: Bearer ${FERRY_KEY}"
```

然后将 `<logical-model>` 替换为上一步返回并已配置路由的模型：

```bash
curl -fsS http://localhost:17810/v1/chat/completions \
  -H "Authorization: Bearer ${FERRY_KEY}" \
  -H "Content-Type: application/json" \
  -d '{"model":"<logical-model>","messages":[{"role":"user","content":"Reply with: ready"}]}'
```

看到正常回复后，在 **请求（Requests）** 中打开这次调用，确认逻辑模型、实际线路和最终结果。若失败，从[请求排障](troubleshooting.md)核对 Key、路由、账号和能力证据。

## 5. 把入口交给你的 Agent

在客户端中设置 Ferry 的 Base URL、刚创建的 Key 和逻辑模型名。按客户端使用的协议选择 [Chat、Responses 或 Messages 入口](api.md)，然后运行一次你熟悉的任务。

接下来进入[长任务指南](long-tasks.md)：为同一模型名增加备用路由，设置恢复条件和首输出等待上限，再验证工具调用与续轮。只有一条正常线路时可以开始使用；添加经过验证的备用线路，才能在首选不可用时多一条路。

想让 Agent 帮忙检查配置或诊断请求，可以安装 [Ferry Skill](agent-skill.md)。

## 6. 停止与持久数据

```sh
docker compose stop
docker compose up -d --no-build
```

停止和重建容器不会删除命名卷。数据库位于卷内 `/data/state`：`control.db` 保存配置与凭据，`conversation.db` 保存会话，`ledger.db` 保存请求与用量。

不要使用 `docker compose down --volumes` 作为日常停止命令，也不要直接复制运行中的数据库。备份、校验和恢复见[Docker 维护指南](docker.md#备份与恢复)。开发环境和测试命令见[开发指南](../development.md)。
