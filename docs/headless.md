# 无界面运行

本页适用于 1.1.0。审阅阶段仍是候选，发行前请继续使用已发布版本；1.0.0 不包含这些参数。

Ferry 可以只启动推理服务。普通二进制增加 `-headless` 参数；已有 SQLite 配置和数据目录继续使用。

```sh
ferry -headless -data-dir ./data
```

此模式不监听管理 HTTP、不提供 Web 控制台，也不启动告警轮询、版本检查、供应商目录同步、定时消息、自动额度重置和参考价格同步。鉴权、路由、配额保护、请求账本、会话续接、冷却持久化及账号令牌刷新继续工作。与 `-standby` 同时使用时，候选环境仍禁止账号刷新。

配置可在停机时通过既有配置备份/恢复流程准备；首次安装可使用一次性 `-config` 导入。无界面模式没有在线管理入口，要管理账号和路由可停止实例后用普通模式打开同一数据目录，再停止并恢复无界面运行。不得让两个进程同时使用同一数据目录。不要把未配置的空实例当成已经可以调用上游。

如果还需要从二进制中移除控制台资源，可以构建专用版本：

```sh
go build -tags headless -o ferry-headless ./cmd/ferry
./ferry-headless -data-dir ./data
```

该构建默认且强制无界面运行，不能通过 `-headless=false` 启用管理 HTTP。此处是开发构建方式，专用预构建发行产物尚未发布；已有 Docker 快速安装仍应使用正式预构建镜像。

`-startup-check -headless` 会将 `management_addr` 返回为空，表示没有管理监听地址。数据面的健康检查和推理端口保持原配置。

无界面运行用于减少服务与后台任务；吞吐、延迟和资源收益应以实际压测为准，不能从是否包含网页推算。

## Docker 使用方式

1.1.0 的普通预构建镜像支持运行时 `-headless`，不需要重新编译。先按 [Docker 指南](guide/docker.md) 完成管理员、上游、路由与推理 Key 配置，再停止普通实例。下载同版本源码中的 `deploy/compose.headless.yaml`，使用相同命名卷启动：

```sh
docker compose down
curl -fsSLo compose.headless.yaml https://raw.githubusercontent.com/hulkbig/ferry/v1.1.0/deploy/compose.headless.yaml
docker compose -f compose.headless.yaml up -d --no-build
```

不要执行 `down -v`。该文件使用默认新安装卷 `ferry-data` 和 `/data/state`；旧 `deploy/compose.yaml` 使用 `/data`，不能未经检查直接切换。自定义卷通过 `FERRY_DATA_VOLUME` 指定。只发布推理端口，不发布管理端口；恢复管理时先停止 headless 实例，再用原 Compose 启动。所有重启均需安排请求排空。

运行时 headless 不等于镜像中删除 Web 资源。本次 Docker 镜像包含普通二进制；专用 `-tags headless` 构建用于实验和开发，不宣称已提供单独 headless 镜像标签。
