# Native Google and Devin integration status

Updated 2026-10-23. Runtime, management API and Provider UI are integrated for
Vertex OpenAPI, Antigravity and Devin. Verification uses offline fixtures; live
login, account entitlement, inference availability and production rollout remain
unverified. This document distinguishes implemented paths from upstream features
that Ferry does not implement.

## Verified source and attribution

Reference: [CLIProxyAPI `d946c4fba6fc9a5cfbca1f3d85184d62ad2fb683`](https://github.com/router-for-me/CLIProxyAPI/tree/d946c4fba6fc9a5cfbca1f3d85184d62ad2fb683),
This identifies the inspected revision; it is not a claim about the latest upstream version.

Upstream paths below are relative to that revision. Ferry paths are relative
to this repository. Endpoint/schema observations are not live-service guarantees.
Vertex uses an independent implementation of Google's service-account protocol;
Antigravity uses independently written protocol handling. Narrowly adapted Devin
pieces retain CLIProxyAPI's MIT attribution and license notices in the source.
No external CLIProxyAPI process, management key or runtime bridge is required.

## Current coverage

| Path | Current Ferry implementation | Limits |
| --- | --- | --- |
| Gemini API key | Existing Google OpenAI-compatible API-key preset | Not a general native generateContent or Gemini Interactions API |
| Vertex OpenAPI | Service-account import, RS256 JWT exchange/renewal, account-bound project/region endpoint, existing Chat caller | Explicit model IDs; no automatic catalog; Google IAM, billing and model access remain live-unverified |
| Vertex Gemini / Interactions | Not implemented as native wire variants | OpenAPI authentication does not establish generateContent, Interactions or Imagen support |
| Antigravity | Operator-client PKCE login, project discovery/onboarding, refresh, Cloud Code request/response translation | Text/function tools; signed/stateful continuation, media and unsupported extensions are rejected |
| Devin | Browser PKCE login, persisted session, Connect/protobuf transport and Chat conversion, stream/nonstream delivery | Explicit model UID; session expiry requires login; media and unsupported request controls are rejected |
| AI Studio browser | Not implemented | Requires an authenticated browser relay runtime; a Gemini API key is not this channel |

The default native registry has seven families and eight IDs: `claude`, `kimi`,
`kimi-ai`, `xai`, `meta`, `vertex`, `antigravity`, `devin`. Codex retains its separate
existing OAuth lifecycle. The list shown in the UI comes from the backend registry.

## Integrated account and UI lifecycle

`internal/nativeauth/defaults.go` registers drivers. The manager persists secrets
and one-shot login sessions, serializes refresh, and exposes only public View
fields through `internal/management/native_oauth_api.go`. Credentials are referenced
as `native-oauth:<driver>:<account>`; raw tokens, private keys, verifiers and session
Extra are not Provider configuration or public status data.

The custom Provider editor supports driver/account selection, explicit login and
completion, safe browser URL/user-code presentation, and live status recovery when
reopened. Device checks use explicit POST completion; GET status is read-only.
Switching driver/account clears credentials and model evidence and invalidates old
requests. Callback and sensitive setup fields are cleared on submission. Closing,
switching or signing out prevents stale responses from restoring the old account.
Login changes account credentials without publishing Provider config; Save/Create
is a separate configuration action.

Vertex accepts service-account JSON and location. The UI explicitly defaults
location to `us-central1`; the driver itself requires a valid supplied location.
Info has no projectless BaseURL. After completion, View exposes the validated
account-bound `base_url` and `protocol`. Antigravity requires the operator's
matching authorized `client_id` and `client_secret`, without environment import.
Devin generates its own honest Ferry client/device metadata; no obscure device
setup fields or imitated upstream client identity are required from the user.

`key_pending` represents Meta device authorization whose inference-key minting did
not finish; explicit completion retries minting without replaying the device grant.
`relogin_required` represents a durable interrupted/failed rotating refresh;
uncertain refresh tokens are not replayed. Both states have dedicated UI actions.

Antigravity, Devin and Vertex default model synchronization to `off` and require
explicit model IDs. Their native catalog is unsupported; configured-model probes
remain available. No guessed models or capability defaults are introduced by login.
The selected native protocol alone is probed for these credentials. Other client
protocols retain Ferry's existing conversion policy.

## Vertex OpenAPI

The implemented credential, validation, JWT, regional/global binding and HTTP
limits are detailed in [Vertex notes](native-auth-vertex-notes.md).
`internal/nativeauth/vertex.go` exposes Binding and ValidateEndpoint without
changing Driver. The runtime checks the configured endpoint against the saved
project/location binding before attaching the bearer. The operation is
`POST <bound-base>/chat/completions`.

Reference observations come from `internal/auth/vertex/vertex_credentials.go`,
`internal/auth/vertex/keyutil.go`, `internal/runtime/executor/gemini_vertex_executor.go`
and `internal/runtime/executor/helps/vertex_openapi.go`. The reference also implements
Gemini generateContent and Interactions branches, plus third-party Vertex-key
compatibility. Those branches are not Ferry's implemented service-account
OpenAPI path. ADC, metadata-service credentials, user-OAuth project discovery,
external-account federation and service-account impersonation are not implemented.

## Antigravity

[Antigravity notes](native-antigravity-notes.md) specify the permitted OAuth client,
PKCE/state validation, fixed daily Cloud Code host, bounded project acquisition,
wire conversion and unsupported signed continuation. Login completion may perform
provider-side onboarding for an explicitly entitled tier; status reads never do.
An arbitrary Google OAuth client is not established as entitled to Cloud Code.

`internal/nativeauth/transport.go` resolves the account's token and project, then
wraps its transport with `nativewire.NewAntigravityTransport`. The synthetic Chat
operation is translated to `/v1internal:generateContent` or
`/v1internal:streamGenerateContent?alt=sse`. `internal/egress/native_chat.go` runs
compatibility validation during route preparation. Unsupported fields and signed
output parts fail explicitly rather than being silently removed.

Upstream evidence: `internal/auth/antigravity/auth.go`,
`internal/runtime/executor/antigravity_executor_auth.go`,
`internal/runtime/executor/antigravity_executor_request.go`, and
`internal/translator/antigravity/openai/chat-completions/`.
No source-embedded client secret is included in Ferry configuration or these notes.

## Devin

The driver builds `https://app.devin.ai/auth/cli/continue` with S256 PKCE and bound
state. Manual callback completion exchanges the authorization code at
`POST https://api.devin.ai/auth/cli/token`. The resulting session token is saved in
the secret store. There is no invented refresh-token grant; invalid sessions
require a new login. Profile/quota/catalog helper behavior observed in the upstream
source is not a promise that Ferry implements those services.

The runtime translates the synthetic Chat operation into
`POST https://server.codeium.com/exa.api_server_pb.ApiServerService/GetChatMessage`.
The wire uses literal `Authorization: Basic <session_token>-<session_token>`, not
base64 Basic user/password, plus `Content-Type: application/connect+proto` and
`Connect-Protocol-Version: 1`. `internal/nativewire/devin*.go` handles framed
protobuf, gzip bounds, end-stream trailers, cumulative deltas, text/tools,
usage, cancellation and stream/nonstream delivery. Truncation and terminal errors
cannot become successful partial output. Output signatures are namespaced data,
not proof of a complete signed-continuation implementation.

The request codec rejects unsupported media, response-format/reasoning controls,
server-side continuation and other unrepresentable extensions. Explicit model UIDs
are required; no model-name heuristics establish capabilities. Client metadata is
created from Ferry identity and actual runtime OS/version with a stable random
device identifier, without impersonating another client.

Upstream evidence: `internal/auth/devin/devin_auth.go`, `record.go` in that directory,
`internal/runtime/executor/devin_executor.go`, and
`internal/runtime/executor/helps/devin_{wire,payload,user_turn,models,quota}.go`.

## Features outside current coverage

The reference Gemini executor exposes native generateContent, streamGenerateContent
and countTokens; its Interactions executor uses `/v1beta/interactions` with a distinct
typed lifecycle, tool IDs and continuation semantics. Ferry's existing
OpenAI-compatible Google preset and the new native Chat translations do not expose
that full client API. Retrieval/delete/background operations and signature-bound
continuation are not implied by text/tool conversion.

The reference `internal/runtime/executor/aistudio_executor.go` depends on a live
`wsrelay.Manager` session, with request/stream messages and an authenticated browser.
Ferry has no delivered AI Studio browser companion or relay lifecycle. Importing
a cookie or using an API key cannot substitute for that runtime. Full AI Studio
browser or Gemini Interactions parity is explicitly not claimed.

## Offline evidence and live limits

Driver tests cover state/PKCE, credential import, token exchange, refresh and safe
failure. Manager/management tests cover persistence, permissions, runtime
reconciliation and recovery status. Native wire/egress tests cover supported
request conversion, stream/nonstream output, tools, usage, cancellation and invalid
input. Console tests cover dynamic drivers, direct Views, bindings, explicit checks,
secret clearing and stale responses. These implemented tests use closed fixtures.

Relevant checks are `go test ./internal/nativeauth ./internal/nativewire ./internal/egress ./internal/management`
and `npm test --prefix web/console`.
This documentation cleanup does not rerun those behavioral suites. It verifies
source consistency, links and diff only. Offline implementation evidence does not
verify real provider consent, client entitlement, model access, billing, production
readiness, or unsupported AI Studio/Gemini Interactions features.
