---
name: ferry
description: Operate a Ferry AI gateway through its stable management API and OpenAI/Anthropic-compatible inference APIs. Use when inspecting Ferry health, models, routes, providers, connections, requests, usage, alerts, revisions, drafts, API keys, secrets, OAuth accounts, runtime cooldown/quota state, syncing OpenCodeX Fast declarations, or sending test inference and image requests. Supports trusted-LAN Management Tokens with an administrator-controlled per-token high-risk gate.
---

# Ferry

If this file was opened by URL and the Skill is not installed, follow the adjacent
[INSTALL.md](INSTALL.md) to install the complete bundle from this same instance.
Do not claim readiness until the configured management access check succeeds.

Resolve the Skill directory from the path that loaded this file. Use its bundled client;
the caller's working directory need not contain `scripts/`:

```bash
FERRY_CLIENT="<loaded-skill-directory>/scripts/ferry.py"
python3 "$FERRY_CLIENT" management GET /api/access
python3 "$FERRY_CLIENT" inference GET /v1/models
```

## Configure access

Supply credentials through your trusted secret manager or process environment. Never put real secrets in shell history, command arguments, logs, or the response summary.

```bash
export FERRY_MANAGEMENT_URL="https://ferry-management.example"
export FERRY_MANAGEMENT_TOKEN="<one-time Management Token from Ferry Console>"
export FERRY_DATA_URL="https://ferry.example"
export FERRY_API_KEY="<inference API key>"
```

Management Tokens authenticate only `/api/*`. Inference API keys authenticate only `/v1/*`. The client enforces this separation.

## Operate safely

1. Before management operations, call `GET /api/access` first. Report the token id and whether `allow_high_risk` is enabled; never report the raw token.
2. Read current state before changing it. For configuration work, inspect the resource plus current draft/revision state.
3. Prefer read, draft, validate, and simulate operations. These work with an ordinary Management Token.
4. Treat apply/activation, immediate provider model sync, secrets, credential rotation, API-key lifecycle, OAuth writes, settings writes, cooldown clearing, and quota overrides as high-risk.
5. Perform a high-risk call only when the user has explicitly authorized that concrete target and change. Reuse that authorization within the same task; ask again only if the effective change or target differs. Add `--confirm-high-risk`. The server independently requires the Ferry Console switch for the token.
6. If the server returns `HIGH_RISK_NOT_ALLOWED`, stop. Ask an administrator to enable that token in **Ferry Console → Settings → Security**. Never bypass the gate or substitute another credential.
7. If a mutation outcome is unknown, reread state before retrying. Retry only with evidence that it did not apply, or with documented idempotency for identical input. On `409`, reread the revision and rebuild the proposal; request renewed review only when the effective change exceeds existing authorization.
8. Never use `/api/auth/*`, `/api/management-tokens*`, retired legacy-only paths, or the removed `/api/v2/*` prefix. Token creation, rotation, revocation, deletion, and its high-risk switch are administrator-session-only Ferry Console actions.

## Choose the reference

- Read [management-api.md](references/management-api.md) for the complete endpoint inventory, request shapes, risk classes, and errors, including `codex_usage_reset_auto` to configure automatic use of expiring Codex reset cards (default 20 minutes) through `/api/settings`.
- Read [workflows.md](references/workflows.md) for revision-safe routing, provider, credential, and API-key procedures.
- Read [data-plane.md](references/data-plane.md) for Chat Completions, Responses, Messages, Images, model listing, and streaming.
- For missing OpenCodeX Fast options or refreshing their declarations, read [opencodex-fast.md](references/opencodex-fast.md) and use the bundled `scripts/sync_opencodex_fast.py`. Preview first; use `--apply` when client configuration updates are in scope. Require evidence from the specific account or endpoint; never add true merely to expose a Fast option. Keep wire tier, raw echo and confirmation separate: Codex echoes cannot confirm or deny acceleration. Opt in to `--sync-tier-authority` only with an OpenCodeX version supporting `responseTierAuthoritative`. Withdraw unsupported manual flags through the reference's revision-safe workflow.

## Client inputs

Send JSON inline or from a file:

```bash
python3 "$FERRY_CLIENT" management POST /api/models/demo/simulate \
  --json '{"protocol":"openai-chat","stream":false}'

python3 "$FERRY_CLIENT" management PATCH /api/settings \
  --json-file ./settings.json --confirm-high-risk
```

Use `--stream` for SSE. Use repeatable `--form NAME=VALUE` and `--file NAME=PATH` for multipart image edits. Additional non-authentication headers use `--header 'Name: value'`.

## Report results

Summarize the requested outcome, returned resource ids/revision, validation evidence, and any remaining risk. Do not paste raw Management Tokens, API keys, secrets, OAuth documents, or Authorization headers. When an API returns a one-time secret, direct the user to store it securely and avoid repeating it later.

