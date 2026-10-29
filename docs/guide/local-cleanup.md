# 部署副本的本地保留与清理

Ferry receiver 管理部署事务副本，maintenance 模块登记离线恢复前的数据副本。清理不属于推理请求、控制台普通操作或 SSH 发布 Key 的权限；由安装了对应版本 receiver 的主机管理员或已有 scheduled-backup 定时器执行。

## 预览与执行

receiver 的服务目录在构建时绑定。以下命令以管理员安装路径为例，不接受任意清理根目录：

```sh
sudo /usr/local/libexec/ferry-receiver cleanup
sudo /usr/local/libexec/ferry-receiver cleanup --apply
```

默认只预览。预览会建立必要的恢复锁目录/文件，但不删除副本、写清理回执或修改应用数据库。执行时重新读取状态并计算计划，不复用旧预览作为删除依据。命令输出 JSON；部分执行失败时返回非零退出码，并保留已经完成的条目和错误。

可以显式调整本次调用的保留边界：

```sh
sudo /usr/local/libexec/ferry-receiver cleanup \
  --min-age 24h --max-age 168h --max-bytes 34359738368
```

这些参数只影响当前调用。定时任务使用默认边界：最短保留 24 小时、临时副本最长保留 7 天、已登记副本分配空间预算 32 GiB。本地备份数量读取控制台备份策略的 local_copies（1–7，默认 2），不能由清理参数降到零。

## 什么可以回收

| 类别 | 必需证据 | 保留方式 |
| --- | --- | --- |
| 部署备份 | receiver 历史记录及准确 operation ID；committed 或 rolled-back | 保留最近实际存在的 local_copies 个事务副本，以及当前事务引用 |
| 部署前中止的副本 | 准确历史记录为 aborted-before-deploy | 满足最短保留期后，按期限或空间预算回收 |
| 恢复演练副本 | 对应事务已进入上述终态 | 当前事务保留；其他按期限或空间预算回收 |
| 首次部署回滚保留的数据 | rolled-back 且 had_previous=false，与 failed/<operation-id>-data 对应 | 最近事务保留；其他按期限或空间预算回收 |
| 恢复前的数据副本 | maintenance 的完成记录，精确对应 data.pre-restore-<时间戳> | 最近实际存在的 local_copies 份补偿副本保留；其他按期限或空间预算回收 |
| 已验证的定时备份 | backups/scheduled 下准确时间名、verified_readback=true | 保留最近 local_copies 份；未满最短保留期的副本继续保留 |

过期和超过份数的副本先回收，再对其余可回收副本按从旧到新的顺序处理空间预算。受保护副本和最短保留期优先于空间预算；无法降到预算以内时输出 over_budget=true，不能为满足数值上限删除有效回滚点。

## 什么会阻止或排除清理

- receiver.lock 已被占用，或同一数据目录正在离线恢复：拒绝并发执行。锁顺序固定为 receiver 锁、恢复锁；恢复锁位于数据目录旁边，不随数据目录交换而移动。
- 当前发布未完成、历史记录包含未决状态，或恢复 journal 仍为 prepared：不删除副本。不能将未知失败直接视为安全终态。
- 当前事务、保留事务或保留恢复记录引用的源目录：保留，包括引用到备份子目录的情况。
- 缺少归属证据的旧 data.pre-restore-*、未知目录及未验证的定时副本：列为 ownership_unproven，自动清理不处理。
- 损坏的状态、非规范路径、软链接、硬链接、跨设备目录、特殊文件或扫描超过 100,000 条目的副本：失败关闭。删除前重新核对目录指纹，发现变化则停止。

活动 data/、control.db、ledger.db、conversation.db 不在删除路径允许列表内。只有有归属证据的完整副本目录可以被清理。本功能不调用 Docker 清理，不连接 S3，也不列举或删除任何对象历史版本；远端备份仍遵守原有按回执精确删除 payload key 的策略。

## 恢复记录与失败处理

离线 Restore 在交换目录前向 .data.restore-state/ 写入并同步 prepared 记录。目录交换并同步成功后写 completed；可以确认已补偿或未改变原数据的失败写 compensated。记录保留源备份、目标数据、临时目录和恢复前目录的精确路径，不含凭据内容。

进程中断或补偿结果未知时，prepared 记录保留，后续恢复和自动清理都拒绝越过它。管理员应在原恢复流程中核对当前数据、暂存目录和恢复前副本，确认唯一正确状态后再完成对账；不要只因为日期较早就改成 completed 或删除 journal。

升级前留下的副本没有上述记录，不能根据名称自动补造归属。需要人工对照原部署/恢复证据单独处理；本次升级不会自动删除这些历史目录。

## 回执与空间统计

实际执行前写入并同步 receiver-state/cleanup/<ID>.json；每删除一项后更新，结束时记录 completed 或 failed。保留最近 64 份完成回执；失败和执行中回执保留待对账。即使进程中断，重试也按当前状态重新计算；已经不存在的副本不重复计数。

- allocated_bytes / removed_allocated_bytes：按文件系统已分配块统计，区别于数据库逻辑文件大小。
- observed_free_bytes_delta：清理前后文件系统可用空间的实际变化。并发写入、打开的文件或共享数据块会影响它，不承诺与已删除文件的块数相等。
- remaining_allocated_bytes / over_budget：按开始时的分配空间，扣除确认删除完成的副本，计算剩余占用和预算状态。部分删除失败时数值偏保守；无法证明归属的目录未纳入统计，不能把它当作整个磁盘的剩余占用。

定时器在尝试本轮备份前执行本地清理；关闭新增备份、尚未到备份时间或随后 S3 上传失败，不再跳过这一步。存在未决发布/恢复时仍保留现场。

清理与备份的失败边界分开：锁冲突、未决状态或无法验证的恢复记录会阻止后续备份；状态和引用已确认后，旧副本的扫描、删除或回执错误会保留清理失败信息，继续尝试新备份。日志和本轮 JSON 的 `cleanup_error` 报告该错误，不把备份成功写成清理成功。

## 维护与验证

实现位于 internal/receiver/cleanup.go、internal/maintenance/restore_journal.go。相关验证：

```sh
go test -race ./internal/receiver ./internal/maintenance ./internal/backuppolicy ./cmd/ferry ./cmd/ferry-receiver
go build ./...
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/ferry ./cmd/ferry-receiver
```

receiver 升级属于应用交付的一部分，按项目发布流程安装。源码修改或本地测试通过不表示旧 SmartAPI 部署已获得此能力；也不能把绑定 Ferry 仓库、服务目录和状态 schema 的接收器直接覆盖到旧 SmartAPI 服务。
