# 构建与发布

公共发行流程见[发行说明](distribution/releasing.md)，安装与运维见[Docker 指南](guide/docker.md)。

`Product checks` 执行产品测试、秘密扫描、依赖检查、镜像构建与漏洞扫描。`Public release` 对 AMD64 和 ARM64 分别完成原生安装与恢复验收，发布准确提交对应的镜像、SBOM 和 Compose 文件。
