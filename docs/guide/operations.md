# 运维指南

本指南适用于单实例、自托管安装。命令从源码根目录执行；数据目录、端口和文件名按实际实例调整。

## 构建与检查

```sh
go build -trimpath -o ./bin/ferry ./cmd/ferry
./bin/ferry -help
./bin/ferry -version-json
```

原生示例启动后：

```sh
curl -fsS http://127.0.0.1:7890/health
curl -fsS http://127.0.0.1:7890/version
```

`health` 只证明服务可响应，不能证明上游账号或模型可用。升级验收还要核对管理员登录、配置、实际推理和请求记录。没有写入构建元数据的本地程序可能显示 `unknown`；不要把它当作可追溯的正式发行包。

## 容器安装

```sh
docker compose -f deploy/compose.yaml up --build -d
docker compose -f deploy/compose.yaml ps
docker compose -f deploy/compose.yaml logs --tail=100 ferry
```

控制台地址为 `http://127.0.0.1:17910`，推理地址为 `http://127.0.0.1:17810/v1`。容器内分别监听 7900 和 7890，宿主端口只绑定 loopback。

Compose 中的卷键为 `ferry-data`，Docker 实际卷名可能包含 Compose 项目前缀。容器使用 UID 10001、只读根文件系统和可写数据卷，移除了额外 Linux capabilities。`ferry:local` 是本地构建标签，不是已发布的固定版本。

停止服务且保留数据：

```sh
docker compose -f deploy/compose.yaml down
```

不要为日常停止或升级添加 `--volumes`，该参数会删除持久卷。容器内监听所有接口，同网络容器可能访问它；只绑定宿主 loopback 不能隔离不可信的容器网络。

## 数据与日志

`control.db`、`ledger.db`、`conversation.db` 分别保存配置和凭据、请求账本、会话状态。WAL／SHM 文件属于活动数据库状态。每个目录只允许一个实例使用，并放在服务用户可写的本地文件系统上。

日志、请求详情和备份均可能包含敏感信息。开启 Debug 请求／响应采集前，确认用途、访问权限和保留时间；自动脱敏不能保证识别正文中的所有秘密。

## 备份

使用内置在线备份，并为每次备份选取新目录：

```sh
umask 077
./bin/ferry -data-dir ./data -backup-to ./backups/before-upgrade
./bin/ferry -verify-backup ./backups/before-upgrade
```

默认仅备份当前生效配置、管理员、API Key、OAuth／上游凭据和运行设置；不包含旧配置版本、草稿、登录会话、管理日志、历史请求和会话正文。控制库直接按表导出，不先复制全库；账本和会话库只建立空结构。定时备份固定使用相同范围。确实需要一次性迁移完整历史时，显式添加 `-backup-history`，例如 `./bin/ferry -data-dir ./data -backup-to ./backups/full-migration -backup-history`。

程序执行 SQLite 备份和完整性检查。不要直接复制运行中的 `.db` 文件。校验成功后，将备份复制到独立保护的存储；同盘或同卷备份不能防范整个存储丢失。

容器内使用同样的程序：

```sh
docker compose -f deploy/compose.yaml exec ferry /usr/local/bin/ferry \
  -data-dir /data -backup-to /data/backups/before-upgrade

docker compose -f deploy/compose.yaml exec ferry /usr/local/bin/ferry \
  -verify-backup /data/backups/before-upgrade
```

选择新的备份目录后再执行下一次备份。保留期限和外部复制由部署者安排；备份默认不加密。

使用发布 receiver 的部署，可按[本地副本清理说明](local-cleanup.md)预览保留与回收计划。普通手工备份和缺少归属记录的历史副本不参与自动清理。

## 恢复演练

恢复到空的新目录，不覆盖正在运行的数据：

```sh
./bin/ferry -data-dir ./restored-data -restore-from ./backups/before-upgrade
./bin/ferry -data-dir ./restored-data -startup-check
```

备份包含监听地址、身份与上游凭据。先核对恢复后的地址，在隔离环境启动，或先停止原来的本地演练实例，避免端口冲突和重复后台调用。启动后验证登录、配置、Key 和一条受控推理请求。配置备份恢复后历史列表为空；只有显式完整备份才恢复历史。

恢复命令成功仅表示数据已恢复，不等于服务和客户端已恢复可用。

## 升级与失败处理

1. 记录当前程序版本、配置 revision 和运行方式，保留旧程序。
2. 创建、校验备份，并确认恢复演练可用；需要保留历史时显式添加 `-backup-history`。
3. 停止原实例，将新程序指向正确的数据目录后启动。
4. 核对版本、健康、登录、配置、实际请求及账本。
5. 如果失败，停止新程序。只有确认数据库兼容时才直接恢复旧程序；否则将升级前备份恢复到新目录，再用旧程序启动。

从配置备份恢复不会保留任何请求或会话历史；从完整备份恢复会丢失备份之后写入的记录。软件回退不会撤销上游已经处理或计费的请求，也不自动回退数据库迁移。

## 远程访问

管理端通过受限制的网络入口访问，并使用 TLS。首次启动应在受控环境初始化管理员，完成前不要开放访问。为客户端分配独立推理 Key，自动化使用专用 Management Token。

Ferry 使用部署者提供的上游凭据，不要求某一种秘密管理产品。具体权限与数据风险见[安全模型](../security-threat-model.md)。

宿主管理员可执行 `ferry-receiver scheduled-backup --now`，立即按已保存的目的地和保留策略备份并回读校验。该命令只跳过到期时间，不绕过停用开关、发布／恢复锁或仅配置范围；SSH 发布 Key 无权调用。
