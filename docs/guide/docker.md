# Docker 安装

[English](docker.en.md)


## 两步启动

需要 Docker Engine 和 Compose。Mac 上已有 OrbStack 或 Docker Desktop 即可，无需重复安装。镜像支持 Linux AMD64 和 ARM64，Docker 会选择对应架构。

在空目录执行：

```sh
curl -fsSLo compose.yaml https://github.com/hulkbig/ferry/releases/download/v0.1.1/compose.yaml
docker compose up -d
```

打开 **`http://localhost:17910`** 创建管理员。无需下载源码、编译程序、填写 `.env` 或安装数据库。

1. 在 **渠道** 添加自己的上游和凭据。
2. 在 **模型与路由** 检查逻辑模型及连接。
3. 在 **API 密钥** 创建客户端 Key；客户端 Base URL 填 `http://localhost:17810/v1`，模型填写刚配置的逻辑模型名。

第一次推理示例见[快速上手](quickstart.md)。Ferry 不附带上游账号或额度。

### 安装在服务器上

端口默认只允许本机访问。先在自己电脑上开启 SSH 隧道：

```sh
ssh -N -L 17910:127.0.0.1:17910 -L 17810:127.0.0.1:17810 user@your-server
```

然后仍打开 `http://localhost:17910`。其他设备长期接入时，完成管理员初始化后再配置 HTTPS 反向代理；推理和控制台分别转发到 `17810`、`17910`。不要直接公开未初始化的管理端口。

### 只用 Docker 命令

```sh
docker run -d --name ferry --restart unless-stopped \
  -p 127.0.0.1:17910:7900 -p 127.0.0.1:17810:7890 \
  -v ferry-data:/data --read-only --tmpfs /tmp:size=32m,mode=1777 \
  --cap-drop ALL --security-opt no-new-privileges:true \
  bighulk/ferry:0.1.1
```

选择 Compose 或 `docker run` 其中一种，不要同时用两种方式启动同一份数据。

## 数据和日常操作

命名卷 `ferry-data` 挂在 `/data`，数据库位于 `/data/state`。管理员、上游凭据、配置和请求记录都保存在卷中，重建容器不会丢失。镜像以 `10001:10001` 运行；数据卷应放在本地磁盘，每份数据库只运行一个实例。

```sh
docker compose ps
docker compose logs --tail=100 ferry
docker compose stop
docker compose up -d
```

不要在日常停止或升级中使用 `docker compose down --volumes`，它会删除数据。`/health` 只表示服务响应正常，上游是否可用需要一次真实请求验证。

## 备份与恢复

每次备份使用新目录名，例如 `before-upgrade`：

```sh
docker compose exec -T ferry /usr/local/bin/ferry -data-dir /data/state -backup-to /data/backups/before-upgrade
docker compose exec -T ferry /usr/local/bin/ferry -verify-backup /data/backups/before-upgrade
umask 077
docker compose cp ferry:/data/backups/before-upgrade ./backup-before-upgrade
```

默认备份仅包含当前配置、管理员、API Key、OAuth／上游凭据和运行设置，不包含历史请求、会话、旧配置版本、草稿或登录会话。备份仍含敏感凭据，应另存到受保护的独立存储。不要直接复制运行中的数据库文件。

需要在升级或迁移后保留完整历史时，使用新目录并显式添加 `-backup-history`：

```sh
docker compose exec -T ferry /usr/local/bin/ferry -data-dir /data/state -backup-to /data/backups/full-migration -backup-history
docker compose exec -T ferry /usr/local/bin/ferry -verify-backup /data/backups/full-migration
```

完整备份同样需要复制到独立存储。定时备份固定为仅配置范围，不能代替完整历史备份。

恢复卷中已经验证过的备份：

```sh
docker compose stop ferry
docker compose run --rm --no-deps ferry -data-dir /data/state -restore-from /data/backups/before-upgrade
docker compose up -d
```

恢复会用所选备份替换当前数据，备份之后的写入不会合并。配置备份恢复后历史列表为空；只有显式完整备份才恢复备份时点的历史。恢复命令失败时先检查原因，不继续启动。完成后验证登录、配置和一次真实请求。卷根目录保存恢复日志和被替换的数据，不要在未验收前删除。

## 升级

先备份并校验；若需要保留历史，使用 `-backup-history` 并完成恢复演练。阅读目标版本说明，再把 `compose.yaml` 中的镜像版本改成目标版本，执行：

```sh
docker compose pull
docker compose up -d
```

正式标签 `0.1.1` 不覆盖，`latest` 会随正式版更新；建议固定版本。需要快速指定版本时，可用 `FERRY_IMAGE=bighulk/ferry:目标版本 docker compose up -d`，随后同步修改 Compose 文件，避免下次启动回到旧版本。

首次公开版没有更早的公共版本可供升级验证。旧内部安装可能把数据库直接放在 `/data`，不能直接套用本页的 `/data/state` 布局；需按[迁移指南](migration.md)备份、恢复并验收。回退镜像不能撤销数据库迁移。

## 常见问题

- **端口占用**：修改 Compose 左侧宿主端口；容器端口 `7900` / `7890` 保持不变。
- **首次登录后反复退出**：本机访问使用 `localhost`；远程管理使用 HTTPS 或 SSH 隧道，不通过裸 IP 的 HTTP 地址登录。
- **镜像拉取失败**：检查 Docker Hub 网络和标签是否已经发布；安装不依赖 OpenBao 或任何私人基础设施。
- **模型不可用**：检查 Key 的模型权限、上游余额、模型 ID 和连接状态。可见模型目录不等于可完成推理。

问题反馈见 [GitHub Issues](https://github.com/hulkbig/ferry/issues)，请提供版本、架构与脱敏复现。

[更多常见问题](faq.md) · [请求排障](troubleshooting.md) · [贡献与 PR](../../CONTRIBUTING.md)
