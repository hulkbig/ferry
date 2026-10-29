# 原生账号驱动参考

这里记录驱动的认证、凭据生命周期和协议限制，供接入排障和实现维护使用。当前驱动已接入控制台与运行链路，测试使用离线 fixture；不据此承诺真实账号准入、模型权限或长期服务可用性。

| 驱动 | 参考 |
| --- | --- |
| Claude、Kimi 国内／国际 | [登录、刷新与端点](native-auth-claude-kimi-notes.md) |
| xAI、Meta | [设备授权、发现与恢复状态](native-auth-xai-meta-notes.md) |
| Vertex | [服务账号、项目与区域绑定](native-auth-vertex-notes.md) |
| Antigravity | [OAuth 客户端与 Cloud Code 适配](native-antigravity-notes.md) |
| Google、Devin | [实现范围与未支持路径](native-google-devin-integration.md) |

Codex 使用独立 OAuth 生命周期，接口见[管理 API](../../../skills/ferry/references/management-api.md)。用户入口和配置选择见[渠道支持](../../provider-channel-coverage.md)。
