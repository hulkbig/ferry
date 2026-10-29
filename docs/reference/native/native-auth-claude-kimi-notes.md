# Native Claude and Kimi authentication integration

Recorded 2026-10-23. These drivers implement the unchanged `internal/nativeauth/types.go`
contract directly in Go. No bridge, provider SDK, subprocess, account files, or real
account calls are involved. Constructors: `NewClaudeDriver() Driver` and
`NewKimiDriver(international bool) Driver` (`false` = kimi.com, `true` = kimi.ai).

## Protocol evidence and provenance

Verified reference: [CLIProxyAPI `d946c4fba6fc9a5cfbca1f3d85184d62ad2fb683`](https://github.com/router-for-me/CLIProxyAPI/tree/d946c4fba6fc9a5cfbca1f3d85184d62ad2fb683), This is the inspected revision, not a claim about the latest upstream version.
Relevant files within that export:

- `internal/auth/claude/anthropic_auth.go`: OAuth URLs, public client ID, scopes,
  PKCE exchange and refresh fields, optional profile/roles companion calls.
- `internal/auth/kimi/kimi.go` and `token.go`: regional hosts, public client ID,
  device grant, headers, persisted device ID and token fields.
- `internal/runtime/executor/helps/kimi_responses.go`, `kimi_executor.go`, and
  `claude_executor_request.go` / `claude_executor_execute.go`: inference endpoints
  and authentication headers used by the native transport.

This is an independently written implementation of the observed protocol constants
and wire fields, not copied implementation code. The reference is MIT licensed;
its copyright notice reads `Copyright (c) 2025-2005.9 Luis Pater` and
`Copyright (c) 2025.9-present Router-For.ME` (first date reproduced as written).
No upstream TLS fingerprinting, fake Axios/Claude Code identity, cloaking, client
metadata fabrication, retry machinery, or logging code is included. Any future
copying of upstream implementation must retain its MIT notice and license text.

## Manager contract

The manager supplies random `LoginOptions.State` and `Verifier`; both are preserved
unchanged in `Session`. Claude requires a nonempty state and RFC 7636 verifier
(43–128 unreserved ASCII characters). Kimi requires state for session bookkeeping;
its device grant does not send a PKCE verifier/state to the provider.

Claude Begin constructs a browser URL without HTTP. Complete accepts the full fixed
callback URL, `code=...&state=...` (optional leading `?`), or `code#state`. Bare codes,
wrong/missing/duplicate states, OAuth error callbacks, and foreign callback URLs
are rejected before HTTP. State is compared in constant time against the manager's
session. Redirect URI is fixed to `http://localhost:54545/callback`; no configurable
redirect/client/token endpoint is accepted. The Provider editor collects the full
callback/query or code#state and clears it on submission; these drivers do not
open a browser or bind a listener. Sessions expire after
15 minutes. The manager consumes successful sessions to prevent replay.

Kimi Begin accepts optional `Fields["device_id"]`; otherwise it generates a random
UUID v4. The same ID is kept in `Session.Extra` and `Token.Extra`. Domain is also
bound there and checked on Complete/Refresh, preventing com/ai credential mixing.
Verification links must use HTTPS and the exact selected region's bare, `www`,
or `auth` host, without userinfo, explicit ports or fragments. The returned complete
URI is preferred, with `verification_uri` fallback. Device/user codes and a positive
expiry are required. Session lifetime is capped at 15 minutes; poll interval is at
least 5 seconds. Complete performs **one** poll and ignores its callback argument.

`ErrPending` and `ErrSlowDown` are returned directly for OAuth pending/slow_down,
including HTTP 200 bodies. The manager retains the session, enforces the next allowed poll time, and adds
5 seconds to the interval after slow_down. The UI polls only through the explicit
Check sign-in action; GET status does not advance authorization. HTTP 429 maps to
ErrSlowDown. Access denial, expired/invalid grant/token, HTTP 401/403, and expired
sessions require a new login. Unknown OAuth errors and malformed responses are
static sanitized errors; provider descriptions, bodies and transport errors are
never returned or logged. No automatic retries: a lost refresh response can mean
that a single-use refresh token was already rotated. The manager serializes refreshes per credential and persists rotation intent
before I/O. Ambiguous refresh failures become durable `relogin_required`; the UI
offers a fresh login without replaying the uncertain refresh token.

Refresh preserves the prior refresh token when the provider omits a replacement,
preserves identity/metadata, and allocates a fresh Extra map. Missing token expiry
remains unknown (zero time), not an invented expiry; negative or >366-day lifetimes
are rejected. Claude identity uses account email, then UUID when supplied; refresh
preserves previous identity if neither is returned. Kimi does not invent an identity
or parse unverified JWT claims.

## Fixed endpoints and egress headers

| Driver | Authentication endpoints | Info / inference integration |
| --- | --- | --- |
| Claude | Browser `https://claude.ai/oauth/authorize`; POST `https://platform.claude.com/v1/oauth/token` for code exchange and refresh | ID `claude`; protocol `anthropic`; base `https://api.anthropic.com/v1`; observed inference POST `/v1/messages?beta=true` |
| Kimi com | POST `https://auth.kimi.com/api/oauth/device_authorization` and `/api/oauth/token` | ID `kimi`; protocol `openai-chat`; base `https://api.kimi.com/coding/v1`; POST `/coding/v1/chat/completions` |
| Kimi ai | POST `https://auth.kimi.ai/api/oauth/device_authorization` and `/api/oauth/token` | ID `kimi-ai`; protocol `openai-chat`; base `https://api.kimi.ai/coding/v1`; POST `/coding/v1/chat/completions` |

Authentication requests use `Accept: application/json` and honest
`User-Agent: Ferry/nativeauth`. Claude sends JSON; Kimi sends
`application/x-www-form-urlencoded`. Both carry the fixed public OAuth client ID
in their bodies. Claude refresh includes the same scope as authorization.

Kimi auth and runtime inference egress carry a stable `X-Msh-Device-Id` from the
saved token and the observed `X-Msh-*` header family. This implementation uses
`X-Msh-Platform: Ferry`, `X-Msh-Version: nativeauth`,
`X-Msh-Device-Name: Ferry` (application device label, not leaked hostname), and
`X-Msh-Device-Model: <actual GOOS> <actual GOARCH>`. The native inference transport
uses version `native` and `User-Agent: Ferry/native`, retaining the saved device
ID. Neither path impersonates another client.

The integrated native transport attaches
`Authorization: Bearer <AccessToken>` for both providers, with JSON Content-Type
and the appropriate JSON/SSE Accept header. Claude OAuth also carries
`anthropic-version: 2023-06-01` and the observed OAuth beta
`anthropic-beta: oauth-2025-04-20`, plus `beta=true` on the request URL.
Additional feature beta flags must be selected
from the actual request/capability evidence. Do not treat OAuth as an `x-api-key`.
Do not fake Claude Code UA or fingerprint to satisfy upstream client restrictions.
Authentication unit tests do not establish that inference succeeds with these
honest headers; live provider compatibility remains unverified.

The reference also exposes optional Claude GET profile
`https://api.anthropic.com/api/oauth/profile` and roles
`https://api.anthropic.com/api/oauth/claude_cli/roles` (Bearer auth). These advisory
lookups are not called by these drivers; token-response identity may be absent.
Kimi reference also has Responses and Anthropic-compatible helpers; Info deliberately
advertises only the observed Chat contract here, without claiming model capabilities.

All HTTP uses the supplied client's transport/egress configuration. A local client
copy disables cookies and **all redirects**, and caps the client timeout and request
context at 30 seconds (earlier caller timeout/deadline wins). Response bodies are
limited to 1 MiB plus one overflow-detection byte and always closed. The client is
not mutated. Caller-provided transports must honor request cancellation, as Go's
HTTP contract requires; this cannot forcibly terminate a malicious RoundTripper.
The management API exposes an allowlisted View, not Session (verifier/device code)
or Token. Transport instrumentation must retain that secret boundary. Only the intended
browser URL/user code should be exposed during login.

## Credential import boundary

The integrated UI provides Claude browser login and Kimi device login; it does
not offer raw token import for these drivers. The following normalization contract
documents credential compatibility, not an available import endpoint. Any import
consumer must validate and store credentials in the secret store, not config exports
or chat/logs.

| Provider | Required normalized import fields | Optional mappings |
| --- | --- | --- |
| Claude | `access_token`; `refresh_token` for renewable credentials | `expires_at` RFC3339 (map reference `expired`); `identity` from reference `email`; Extra `account_uuid`, `organization_uuid`, `token_type=Bearer` |
| Kimi | `access_token`, explicit selected provider/region, stable `device_id`; `refresh_token` for renewal | `expires_at` RFC3339 (map `expired`); Extra `scope`, `token_type=Bearer` |

For Kimi, normalize the selected region to `Extra["domain"]` exactly `kimi.com` or
`kimi.ai`, and device ID to `Extra["device_id"]`. Refresh rejects missing/wrong domain
or device ID; do not silently regenerate IDs on refresh/import. Reference `type`
values are `kimi` and `kimi-ai`. If imported `type`, `domain`, or `base_url` disagree,
reject instead of guessing. Imported base URLs never override fixed auth endpoints.
A refresh-only credential cannot be used as an inference access token. Missing
refresh token is usable only as an explicitly nonrenewable access credential;
missing expiry is unknown, not proof of validity. Keep imported tokens/control
characters bounded and reject non-Bearer token types.

## Local verification

Closed fake RoundTripper tests cover both Kimi regions, Claude PKCE/callback state,
request fields/headers, token refresh/rotation, metadata preservation, one-shot
poll errors, expiry, domain/verification-link rejection, malformed/oversized bodies,
redirect blocking, sanitized errors and cancellation. No network/account fixtures.

Commands: `go test ./internal/nativeauth`, `go test -race ./internal/nativeauth`,
`go build ./internal/nativeauth`, `go vet ./internal/nativeauth`.
These are local module checks, not a production login/inference or release gate.

Info BaseURL includes `/v1` because callers append
`/messages` or `/chat/completions` directly. Token endpoints are unchanged.

The default manager registers `claude`, `kimi` and `kimi-ai`. The Provider editor
loads registered drivers, recovers status on reopen, and saves only the connected
credential reference to its draft. Native endpoint/protocol selection is fixed;
other ingress protocol conversion uses the existing policy. Catalog reads attempt
`/models`; capabilities still require explicit probes, not login success.
