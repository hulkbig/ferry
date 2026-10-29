# API 接入

继续使用熟悉的客户端，把推理地址、Key 和模型名指向 Ferry。Chat、Responses 和 Messages 共用逻辑模型与路由管理，保留各自的请求格式。先用客户端原生支持的协议完成一次调用，再验证工具和流式行为。

以下例子使用本机原生启动的地址。Compose 用户将 `7890` 换为 `17810`。客户端的 Base URL 通常填写 `http://127.0.0.1:7890/v1`；需要单独填写完整请求地址的客户端按下表配置。

## 入口

| 方法与路径 | 用途 |
| --- | --- |
| `GET /v1/models` | 当前 Key 可见的模型目录 |
| `POST /v1/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses` | OpenAI Responses |
| `POST /v1/messages` | Anthropic Messages |
| `POST /v1/images/generations` | 原生图片生成 |
| `POST /v1/images/edits` | 原生图片编辑 |

图片入口不会通过文本协议转换来补齐能力。请求格式、模型权限和图片能力必须符合所选上游。管理端的 `/api/*` 使用独立身份，见[管理 API](../../skills/ferry/references/management-api.md)。

## 准备凭据

先在控制台创建推理 Key，并配置可用 Logical Model。在 Bash 或 Zsh 中隐藏输入 Key，避免把值写进命令历史：

```bash
printf 'Ferry key: '
read -r -s FERRY_KEY
printf '\n'
export FERRY_KEY
```

```sh
curl -fsS http://127.0.0.1:7890/v1/models \
  -H "Authorization: Bearer ${FERRY_KEY}"
```

下面的 `<logical-model>` 替换为已配置的模型名。相同模型不一定有三种协议的可用路由，应按实际配置选择。

## Chat Completions

```sh
curl -fsS http://127.0.0.1:7890/v1/chat/completions \
  -H "Authorization: Bearer ${FERRY_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{"model":"<logical-model>","messages":[{"role":"user","content":"Reply with ready."}]}'
```

## Responses

```sh
curl -fsS http://127.0.0.1:7890/v1/responses \
  -H "Authorization: Bearer ${FERRY_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{"model":"<logical-model>","input":"Reply with ready."}'
```

## Anthropic Messages

```sh
curl -fsS http://127.0.0.1:7890/v1/messages \
  -H "Authorization: Bearer ${FERRY_KEY}" \
  -H 'anthropic-version: 2023-06-01' \
  -H 'Content-Type: application/json' \
  -d '{"model":"<logical-model>","max_tokens":128,"messages":[{"role":"user","content":"Reply with ready."}]}'
```

## 流式请求与工具

在相应文本请求中设置 `"stream": true`，并使用 `curl -N` 避免本地输出缓冲。读取每种协议自己的终止事件；HTTP 200 或收到第一帧都不能单独证明请求已成功结束。

工具调用遵循入口协议：Chat 使用 `tool_calls`／工具结果消息，Responses 使用函数调用条目，Messages 使用 `tool_use`／`tool_result` 内容块。续轮时保留协议要求的调用 ID，不要把工具结果当作普通用户文字重新提交。支持范围见[协议行为](../protocol-fidelity.md)。

## 排查失败

先检查请求所用的 Key、模型名、路由和能力，再查看控制台 **Requests** 中的尝试记录。保留请求 ID、时间、协议和脱敏错误；不要提交 Key 或完整业务正文。流式请求开始后出现的错误需要检查事件流和最终记录，不能只看 HTTP 状态。

用完临时终端凭据后可执行 `unset FERRY_KEY`。更多场景见[常见问题](troubleshooting.md)。
