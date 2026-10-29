# Ferry data plane

Set `FERRY_DATA_URL` and `FERRY_API_KEY`. The inference API key is distinct from a Management Token.

## Endpoints

| Method and path | Protocol |
| --- | --- |
| `POST /v1/chat/completions` | OpenAI Chat Completions, including tools and SSE |
| `POST /v1/responses` | OpenAI Responses, including reasoning, tools, continuity and SSE |
| `POST /v1/messages` | Anthropic Messages, including tools, thinking and SSE |
| `POST /v1/images/generations` | OpenAI-compatible native image generation |
| `POST /v1/images/edits` | OpenAI-compatible multipart native image edit |
| `GET /v1/models` | Client-visible logical model list |
| `GET /health`, `/healthz`, `/version` | Public liveness/build identity |

Ferry accepts `Authorization: Bearer` for every `/v1/*` protocol. Standard Anthropic clients may instead send `X-Api-Key`.

## Fast capability discovery

Each model returned by GET /v1/models includes this Ferry extension:

    "supported_service_tiers": {
      "openai-chat": [],
      "openai-responses": ["priority"]
    }

An empty list means no configured safe priority route for that ingress protocol.
Missing metadata means an older/unknown server, not a negative capability result.
Priority requires an enabled declared target in the default route and every enabled override
that can affect the protocol, respecting native/conversion and logical-model exact-only restrictions.
This describes configured support, not transient quota, health, request-specific eligibility or latency.

OpenAI Chat/Responses `service_tier` is trimmed and lowercased before routing. Accepted values are
empty, `auto`, `default`, `flex`, `priority` and `fast`; other values, including `ultrafast`, are rejected.
Both `priority` and `fast` are hard routing requirements and require declared priority capability.
Only Codex maps `fast` to `priority` at the upstream boundary. Other OpenAI Chat/Responses forwarding
preserves the normalized value; incompatible protocol conversions fail.
Fast declarations require specific account catalog metadata or a trustworthy endpoint contract, never a
model-name guess, timing probe, or a manual flag added only to expose the option. Interpret the returned
service_tier according to the upstream contract: public OpenAI API echoes are authoritative; Codex OAuth
echoes are informational (both default and priority remain unconfirmed). Unknown gateways, missing fields
and failed requests cannot confirm priority. Streaming and non-streaming preserve the original echo.
Responses include X-Ferry-Response-Tier-Authority and X-Ferry-Response-Tier-Source for the actual
committed attempt when observed. Model rows separately expose response_tier_authority per OpenAI protocol,
conservatively combining all possible routes. Neither metadata enables Fast. Request/attempt history
includes service_tier_evidence with the serialized wire tier, authority, source and final confirmation.
Anthropic tier/speed semantics are separate.
OpenCodeX 2.43.0 requires a client configuration bridge; see
[the integration guide](opencodex-fast.md).

## Examples

Chat:

```bash
python3 scripts/ferry.py inference POST /v1/chat/completions --json '{
  "model":"smart",
  "messages":[{"role":"user","content":"Hello"}],
  "stream":false
}'
```

Responses SSE:

```bash
python3 scripts/ferry.py inference POST /v1/responses --stream --json '{
  "model":"smart",
  "input":"Explain the current status",
  "stream":true
}'
```

Anthropic Messages:

```bash
python3 scripts/ferry.py inference POST /v1/messages --json '{
  "model":"smart",
  "max_tokens":512,
  "messages":[{"role":"user","content":"Hello"}]
}'
```

Image generation:

```bash
python3 scripts/ferry.py inference POST /v1/images/generations --json '{
  "model":"gpt-image-2",
  "prompt":"A precise isometric system diagram",
  "size":"1024x1024"
}'
```

Image edit uses multipart input. Repeat `--file image=...` when the upstream protocol accepts multiple images.

```bash
python3 scripts/ferry.py inference POST /v1/images/edits \
  --form model=gpt-image-2 \
  --form 'prompt=Remove the background' \
  --file image=./input.png
```

## Semantics

- `model` is a client-facing Logical Model. The request ledger records the selected Provider, Connection, and configured upstream model separately from the model and service tier observed in the upstream response.
- Capability checks fail closed. Ferry uses a native client protocol path first and only performs a lossless configured conversion.
- A route advances only for its typed `recover_on` failures or when it has no eligible connection. Client authentication/parameter errors never trigger upstream fallback.
- Streaming output is passed as SSE. Use `--stream`; do not buffer or reinterpret events.
- Image endpoints require a native `openai-images` connection. Images are not converted through text protocols.
- Continuity headers (for example Codex thread/session or Claude Code session ids) may influence conversation/cache affinity but do not broaden API-key scope.
- `X-Codex-Turn-State` is accepted only on Responses ingress and forwarded only when an existing conversation remains bound to the same Codex Provider and Connection. It is omitted after a rebind or fallback to a different Provider or Connection.
