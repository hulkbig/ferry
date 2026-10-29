# 开发指南

[English](development.en.md)

## 环境与目录

使用 `go.mod` 中指定的 Go 版本，当前为 Go 1.26.10。完整检查还需要 Node.js、npm 和 Python 3；CI 使用 Node.js 22、Python 3.12。控制台为原生 JavaScript／CSS，没有 npm 包安装步骤。

```text
ferry/
├── cmd/ferry/         程序入口、启动与维护命令
├── internal/            配置、管理接口、路由、协议、账号和存储
├── web/console/         控制台源码及行为测试，随二进制嵌入
├── skills/ferry/     Agent Skill、Python 客户端和管理 API参考
├── deploy/              通用 Dockerfile、Compose 与容器启动配置
├── scripts/             产品检查、候选构建与安全扫描
├── docs/
│   ├── guide/           安装、配置、客户端、运维和排障
│   ├── reference/native/ 原生账号驱动的实现参考
│   └── archive/         标明失效范围的历史设计
├── LICENSES/            第三方许可原文和来源记录
└── .github/             通用 CI、问题模板与 PR 模板
```

`docs` 根目录保留功能参考、维护说明和机器可读的依赖清单。许可证清单的位置被构建脚本引用，移动时需要同步检查和打包逻辑。内部 Go module 名仍为 `ferry`，当前可直接构建；命令入口统一为 `cmd/ferry`。

## 请求路径

```text
客户端 → ingress（入口协议）→ policy / scheduler（选路、能力与调度）
       → egress / nativewire（上游协议）→ 供应商
                      ↓
                 ledger（请求与用量）

控制台 / Agent → management → controlstore / secretstore
原生账号生命周期 → nativeauth；Codex OAuth → oauth
```

`conversation` 保存会话续接状态，`runtime` 管理运行态，`billing` 处理定价和用量计算。修改公共协议或配置时，应检查对应入口、出站、管理 API、控制台和 Skill 的调用方。

## 运行与检查

```sh
go run ./cmd/ferry -data-dir ./data -config ./config.example.yaml
```

控制台通过 Go embed 分发。修改 JS／CSS 后需要重新构建并重启本地服务，才能检查实际页面。

按改动范围选择检查：

```sh
# Go 模块示例
go test ./internal/management

# 控制台行为和组装检查
npm test --prefix web/console

# Python 管理客户端
python3 -B -m unittest discover -s skills/ferry/scripts -p 'test_*.py'

# 完整产品检查：声明、Go test/vet/build、控制台和 Skill
bash scripts/test.sh
```

协议、调度、账号并发等修改还应运行受影响包的 race 检查，例如：

```sh
go test -race ./internal/nativeauth ./internal/nativewire ./internal/management
```

测试使用合成数据或模拟服务时，在说明中明确这一点。真实供应商验证需独立账号、明确范围和可追溯的结果，不自动在开发检查中触发。

## 安全检查与候选产物

```sh
python3 scripts/security_scan.py
python3 scripts/check_source.py
bash scripts/check_go_vulnerabilities.sh
```

Docker 扫描工具固定 digest；默认先取得镜像再断网扫描。镜像已准备好时，秘密扫描可显式使用 `--offline`，缺镜像则失败。当前源码静态检查是有限的定向规则，不代表全面安全审计。

[本地候选构建](local-candidate-builds.md)说明三平台二进制、声明材料、校验清单和镜像 SBOM。产物应生成在源码外；发布前按[发布检查](release-readiness.md)重新绑定准确版本和结果。

## 文档写作

- 先写读者要完成的事，再写步骤和适用条件。
- 命令、端口、字段和功能范围以代码、配置和测试为准。
- 区分实现、离线测试、真实账号验证及生产验收，不用一个“支持”替代全部状态。
- 不写无法核实的性能数字、兼容率、用户数或发布日期。
- 调试过程、私人环境和阶段汇报不放入用户指南。保留来源声明，历史设计单独标记。
- 中文主入口和英文 README 的功能与限制保持一致。详细参考可以保留原语言，索引中注明用途。

改动和第三方来源要求见[贡献说明](../CONTRIBUTING.md)。
