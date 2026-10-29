# 从 SmartAPI 迁入 Ferry

Ferry 是该源码候选版的新名称。改名没有改变管理 API、推理协议端点、SQLite 文件名或默认监听端口。原生渠道的实际可用范围仍以[渠道支持表](../provider-channel-coverage.md)为准。

## 程序与数据

新程序从 `cmd/ferry` 构建，命令为 `ferry`。使用同一份已备份的数据目录启动，既有管理员、Key、渠道、账号、配置版本和请求账本继续使用。启动脚本识别 `data/cpa.pid` 中的旧进程，并核对二进制和数据目录；未确认的 PID 不会被停止。

升级前按[运维指南](operations.md)创建备份；需要保留请求、会话和旧配置历史时显式添加 `-backup-history`。默认配置备份不包含这些历史。在独立目录恢复并验证所选范围，再停止旧实例、切换程序。不要让两个进程同时操作同一个数据目录。首次启动可能升级数据库，回退应使用升级前的备份及对应程序。

已有备份继续使用 `smart-api/sqlite-backup/v1`，统计导出继续使用 `smartapi.analytics/v1`；新下载文件名使用 Ferry。新建推理 Key 使用 `sk-ferry-`，旧 Key 仍可认证、轮换和撤销。改名不要求轮换凭据。

## 并行验证与 OAuth 接管

复制数据库不等于复制了一套独立的上游登录。两个实例持有同一个 OAuth refresh token 时，任何一方续期都可能让另一方保存的令牌失效。不要直接把两份数据以正常模式同时运行。

验证恢复副本时使用 `-standby`：

```bash
go run ./cmd/ferry -data-dir ./migration-copy -standby
```

此模式保留已有 Key、配置和有效访问令牌，允许真实推理，但禁止 Codex 与原生账号续期、新登录及兑换 Codex 重置额度，也不恢复未完成的设备登录。定时上游消息、模型同步、Codex 版本更新、自动重置额度和参考价格同步暂停；设置本身不被改写。客户端主动请求仍会消耗上游额度，新旧实例的并发计数也相互独立，应控制验证流量。

访问令牌到期或上游返回 401 时，副本会停止使用需要续期的身份，不会为了通过验证而刷新共享凭据。API Key 上游不受 OAuth 续期限制。备份定时器属于外部部署配置，需要单独禁用或配置独立目标，避免操作原实例的备份目录。

正式接管时，先停止旧实例的请求入口和后台账号任务，再用 `-backup-history` 获取最后一致的完整备份并在新实例恢复，核对配置版本、Key、OAuth 身份、会话和账本，随后去掉 `-standby` 启动新实例并验证。旧实例保留为停机恢复点，不能与新实例同时续期同一授权。新实例已续期后，旧备份中的 refresh token 可能过期；回切需要当前有效凭据或重新登录，不能仅换回旧二进制和旧数据库。

正式发布 Compose 默认启用后台任务。已验证并备份的原数据目录也可以在两个实例均停止时转交给新实例：保留原验证副本，确认新旧实例不再引用同一活动目录，再由正式发布启动并迁移数据。入口切换应保留原 Key，并核对最后配置版本、OAuth 身份、会话和账本；旧地址可在过渡期代理到同一个新后端，避免长期运行两套持有相同授权的网关。

## 容器数据卷

新 Compose 服务名为 `ferry`，镜像为 `ferry:local`，默认卷键为 `ferry-data`。旧 Docker 卷不会自动重命名或复制。改变 Compose project 名会影响默认卷名；直接启动新配置可能得到空实例。

先检查旧容器的实际挂载并备份。在本地创建一个 Compose 覆盖文件，将 `ferry-data` 显式绑定到已确认的旧卷：

```yaml
volumes:
  ferry-data:
    external: true
    name: REPLACE_WITH_CONFIRMED_EXISTING_VOLUME
```

用 `docker compose -f deploy/compose.yaml -f compose.existing-data.yaml config` 检查合并结果。确认数据卷、端口和旧实例已停止后再启动。不要用 `down -v` 处理改名。也可以在新卷恢复备份，完成独立验证后再切换流量。

## Skill 与连接变量

新安装入口为 `/skills/ferry/SKILL.md`，ZIP 为 `ferry.zip`，解压根目录为 `ferry/`，客户端为 `scripts/ferry.py`。旧 `/skills/smartapi/` 仍提供根目录为 `smartapi/` 的兼容包和 `scripts/smartapi.py` 包装入口；两者使用同一客户端实现，ZIP 与对应校验文件配套。

| 新变量 | 兼容变量 |
| --- | --- |
| `FERRY_MANAGEMENT_URL` | `SMARTAPI_MANAGEMENT_URL` |
| `FERRY_MANAGEMENT_TOKEN` | `SMARTAPI_MANAGEMENT_TOKEN` |
| `FERRY_DATA_URL` | `SMARTAPI_DATA_URL` |
| `FERRY_API_KEY` | `SMARTAPI_API_KEY` |

每个平面的 URL 和凭据必须来自同一套命名。仅在新组完全未设置时回退到旧组；两组同时存在时必须一致。缺项、空值、跨组拼接或冲突会报错，不输出凭据。显式 `--base-url` 可替代 URL；健康和版本查询不强制要求 Key。配置由使用者的秘密管理工具注入，不写入仓库。

OpenCodeX 同步必须显式传入已有的 `--provider` ID。该 ID 是用户配置，不随产品改名。例如，原 `smart-api` Provider 可以继续保留；工具会核对所选 Provider 与环境中数据 URL 的 origin，不自动改写 Provider 名称。全局已安装的 SmartAPI Skill、旧服务凭据和路由不会被本项目修改。

## 客户端兼容

以下响应头同时提供 `X-Ferry-` 与 `X-SmartAPI-` 两种前缀，值相同：`Control-Version`、`Warning`、`Response-Tier-Authority`、`Response-Tier-Source`。新客户端优先读取 Ferry 名称。会话 Cookie 和 CSRF Cookie 保留原名，鉴权要求不变。

同一浏览器 origin 下，主题和语言从有效的 `smartapi.theme`、`smartapi.locale` 迁入 `ferry.theme`、`ferry.locale`；新值优先，旧值保留。不同端口或域名不能共享本地偏好。主题默认跟随系统并响应实时变化。

## 备份路径和账号身份

新库的 S3 默认前缀为 `ferry/scheduled`。已有配置继续使用原值；旧库未保存备份策略时，首次启动会固化 `smart-api/scheduled`，避免静默改变备份位置。两种命名空间都保留路径检查，历史远端对象不会自动移动。

旧 Key 派生 ID、Codex 会话和缓存身份、会话证据类型，以及原生账号设备指纹保持不变。Devin 新账号使用 `ferry` 身份，已有 `smart-api` 身份继续复用；其他应用身份仍被拒绝。
