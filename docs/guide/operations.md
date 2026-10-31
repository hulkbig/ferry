# 运维指南

默认安装使用已发布的 Docker 镜像，无需源码或编译工具。完整安装、远程访问和维护步骤见 [Docker 安装指南](docker.md)。

## 安装与检查

在空目录执行：

```sh
curl -fsSLo compose.yaml https://github.com/hulkbig/ferry/releases/download/v1.1.0/compose.yaml
docker compose up -d --no-build
docker compose ps
docker compose logs --tail=100 ferry
curl -fsS http://localhost:17810/health
curl -fsS http://localhost:17810/version
```

控制台为 `http://localhost:17910`，推理 Base URL 为 `http://localhost:17810/v1`。默认只允许本机访问。先创建管理员、接入上游并创建客户端 Key，步骤见[快速开始](quickstart.md)。健康检查不代表上游可调用，还需验证一次实际请求。

## 数据、停止与备份

根目录发行 Compose 使用命名卷 `ferry-data`，数据库位于卷内 `/data/state`。停止保留数据：

```sh
docker compose stop
```

每次备份使用新目录，默认只包含当前配置及恢复凭据，不包含请求和会话历史：

```sh
docker compose exec -T ferry /usr/local/bin/ferry -data-dir /data/state -backup-to /data/backups/before-upgrade
docker compose exec -T ferry /usr/local/bin/ferry -verify-backup /data/backups/before-upgrade
umask 077
docker compose cp ferry:/data/backups/before-upgrade ./backup-before-upgrade
```

备份含敏感凭据，须另存到受保护的独立存储。确实需要完整历史时显式添加 `-backup-history`。不要复制运行中的数据库，不要用 `down --volumes` 停止服务。恢复演练和升级见[备份与恢复](docker.md#备份与恢复)及[升级](docker.md#升级)。

## 旧版 deploy Compose 安装

此前使用 `deploy/compose.yaml` 的实例保留原 Compose 项目、卷和 `/data` 数据布局。该文件现在默认拉取发行镜像，不再本地构建：

```sh
docker compose -f deploy/compose.yaml pull
docker compose -f deploy/compose.yaml up -d --no-build
```

继续使用原来的项目名及卷。不要直接换成根目录 Compose，否则项目／卷名及数据路径可能不同。旧布局的备份参数仍为 `-data-dir /data`；有迁移需求时按[迁移指南](migration.md)执行。恢复旧镜像不能撤销数据库迁移。

## 开发与自动化运维

源码运行和镜像构建仅用于[开发环境](../development.md)，不属于快速安装步骤。发布 receiver 的清理规则见[本地副本清理](local-cleanup.md)；普通手工备份不自动参与回收。

管理入口使用受限网络与 HTTPS，初始化前不要开放。日志、请求正文和备份可能包含敏感信息。Ferry 不依赖某一种秘密管理产品，自动化使用专用 Management Token，客户端使用独立推理 Key。

宿主管理员可执行 `ferry-receiver scheduled-backup --now`，立即按已保存策略备份并回读。该命令只跳过到期时间，仍遵守停用、发布／恢复锁和仅配置范围；SSH 发布 Key 无权调用。
