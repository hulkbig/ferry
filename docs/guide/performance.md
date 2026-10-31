# 资源消耗对比

Ferry 使用 Go + SQLite，支持 headless 模式，无需外部数据库。

### 内存占用

| 项目 | 1 KiB 响应：峰值工作集 | 64 KiB 响应：峰值工作集 |
| --- | ---: | ---: |
| **Ferry** | **30.8 MiB** | **34.5 MiB** |
| CLIProxyAPI | 68.1 MiB | 67.5 MiB |
| sub2api＋PostgreSQL＋Redis | 441.9 MiB | 432.5 MiB |

64 KiB 场景下，Ferry 的工作集比 CLIProxyAPI 低约 **49%**，比 sub2api 整套服务低约 **92%**。

![内存占用对比](../assets/perf-peak_working_set_mib-zh.svg)

### 峰值 CPU 占用率

| 项目 | 1 KiB 响应 | 64 KiB 响应 |
| --- | ---: | ---: |
| **Ferry** | **146.6%** | **157.1%** |
| CLIProxyAPI | 151.0% | 151.2% |
| sub2api＋PostgreSQL＋Redis | 390.2% | 406.3% |

![峰值 CPU 占用率](../assets/perf-peak_cpu_percent-zh.svg)

测试：2026-10-30，ARM64 Docker，并发 32，零额外时延 Mock。每个代理 2 vCPU；sub2api 另配 PostgreSQL 2 vCPU、Redis 1 vCPU，资源数包含依赖。Ferry headless 1.1.0 候选每项 15 秒×3，CLIProxyAPI 8.0.47 为 12 秒×3，sub2api 0.2.19 为 30 秒×1。功能和测试窗口不同；这些数字只描述本次资源测量。内存为工作集，不是 RSS；sub2api 为各组件峰值之和。

## 测试方法

响应大小指 content 字节数，输入文本 128 字节，OpenAI Chat JSON、HTTP/1.1、不压缩。测试机为 Apple M1，Docker VM 8 vCPU、约 8 GiB，非独占环境。一次测试一个代理。

Ferry 保留鉴权、账本、会话及请求轨迹。CLIProxyAPI 使用 commercial-mode，关闭管理和用量聚合。sub2api 使用 simple 模式，保留用量数据库。

工作集按 cgroup usage − inactive_file 计算，约每秒采样；重复测试取各轮峰值中位数。sub2api 各组件峰值不一定同时发生。峰值 CPU：相邻监控样本的容器累计 CPU 时间差 ÷ 采样读取时间差 × 100%；100% 为一个逻辑核。sub2api 先加总同一采样区间的主进程、PostgreSQL 和 Redis，再取每轮峰值；重复测试取各轮峰值中位数。采样窗口包含启动与账本排空观察，排除 Mock 与压测驱动。

[逐轮 CSV](../benchmarks/2026-10-30/measurements.csv) · [完整测量摘要](../benchmarks/2026-10-30/summary.json) · [构建来源](../benchmarks/2026-10-30/provenance.json)

下载数据保留全部测量指标；本页展示三方内存与峰值 CPU 对比。脚本位于 `scripts/perf/`，图表由 `scripts/build_performance_figures.py` 生成。

[CPU 采样区间与计算数据](../benchmarks/2026-10-30/cpu-intervals.json)。由 `scripts/perf/export_peak_cpu.py` 从同一批原始资源样本提取。
