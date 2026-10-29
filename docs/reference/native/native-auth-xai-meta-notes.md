# Native xAI and Meta authentication

Implemented 2026-10-23 against the unchanged `internal/nativeauth/types.go`
contract. Constructors are `NewXAIDriver() Driver` and `NewMetaDriver() Driver`.
The default manager registers both drivers. The native transport supplies their
credentials to the existing Responses caller, and the Provider editor supports
device login and status recovery. Signing in does not publish Provider config
or infer model capabilities.

## Source and attribution

Verified reference: [CLIProxyAPI `d946c4fba6fc9a5cfbca1f3d85184d62ad2fb683`](https://github.com/router-for-me/CLIProxyAPI/tree/d946c4fba6fc9a5cfbca1f3d85184d62ad2fb683), This is the inspected revision, not a claim about the latest upstream version.
Relevant upstream files are `internal/auth/xai/{types,xai}.go`,
`internal/auth/meta/meta.go`, and `internal/runtime/executor/` files
`xai_executor{,_auth,_request,_execute}.go`, `meta_executor.go`,
`meta_executor_execute.go`, and `meta_executor_stream.go`.

The implementation is newly written around Ferry's Driver interface from
protocol observations in those files. No upstream implementation bodies were
copied. Endpoint strings, public OAuth client IDs, scope and grant values,
response field names, and lifecycle semantics come from that source. No live
provider requests or current provider capability verification were performed.

## Sessions, secrets, and polling

`Begin` preserves the supplied `LoginOptions.State` and `Verifier` verbatim.
They are session correlation data; these device protocols do not send PKCE or
browser callback state. `Complete` ignores its callback string and performs one
poll. The caller must protect session ownership and serialize polling.

Use `errors.Is(err, ErrPending)` and `errors.Is(err, ErrSlowDown)`; on pending,
wait at least `Session.IntervalSeconds`, and on slow-down increase the interval
by at least five seconds for all remaining attempts. Drivers do not sleep or
maintain a polling scheduler. Denied/expired grants and invalid grants return
`ErrLoginRequired`. Session expiry is checked before sending the device code.
Device windows are capped at 30 minutes (xAI) and 15 minutes (Meta), with a
minimum/default five-second interval. Missing/invalid device lifetimes fail.

All `Token` values, including `RefreshToken`, belong in the existing secret
store. DeviceCode and Verifier are also secrets; never expose serialized
Session/Token objects in public views or logs. Extra metadata and all partial credential records are treated as confidential
alongside the tokens. Drivers do
not log, persist files, manufacture account identity, or decode unverified JWTs
into identity assertions. Meta's returned email is provider profile metadata,
not an authorization decision.

## xAI

Discovery: `https://auth.x.ai/.well-known/openid-configuration`.
Public client: `b1a00492-073a-47ea-816f-4c329264a828`.
Scope: `openid profile email offline_access grok-cli:access api:access`.
Discovery must provide device and token endpoints on exactly `auth.x.ai` over
HTTPS. If supplied, issuer must equal `https://auth.x.ai`. Discovery endpoints
and stored token endpoints are validated each time, including refresh; no
arbitrary x.ai subdomains, userinfo, ports, query strings, fragments or escaped
paths are accepted. Browser verification hosts are exactly `auth.x.ai`,
`accounts.x.ai`, or `x.ai`; verification query strings are allowed.

The token endpoint is retained in Extra as `token_endpoint`. Refresh uses the
OAuth refresh-token grant, preserves an existing refresh token if rotation is
omitted, and replaces it when returned. A missing refresh token requires login.
If imported credentials lack a token endpoint, refresh performs discovery.
Unknown access-token expiry remains zero; no invented lifetime is assigned.

## Meta minted-key lifecycle

Device authorization: `https://auth.meta.com/oidc/device/authorization/`.
Token polling: `https://auth.meta.com/oidc/device/token/`.
Public client: `1031625952748946`. No scope or fabricated PKCE is added.
Browser verification hosts are exactly `auth.meta.com`, `accounts.meta.com`,
`www.meta.com`, or `meta.com`.

The OAuth access token is a Device Client Access (DCA) credential. It is stored
in **RefreshToken**, never returned as the usable inference AccessToken.
`Refresh` means POST `https://api.meta.ai/muse-code/key` with JSON
`{"dca_token":"<secret>"}` and `Authorization: Bearer <same secret>`; it is not
an OAuth refresh grant. `Complete` polls, then immediately attempts that mint.
The minted `api_key` becomes AccessToken. A returned BaseURL must be
`https://api.meta.ai/v1` (optional trailing slash); omission uses that default.
Other origins and paths fail closed.

The DCA expiry is stored separately as `Extra["dca_expires_at"]` (RFC3339).
When both expires_in and expires_at are supplied, the earlier deadline wins.
Expired or malformed DCA expiry requires login before reminting. A minted key
has no advertised expiry: Token.ExpiresAt stays zero even after DCA expiry.
An existing key is not implicitly revoked or expired by a failed remint.
The caller retains the old token until successful replacement. Refresh without
a DCA credential requires login; no fabricated key refresh/revocation API exists.

If minting fails after device authorization, Complete returns an error **and a
partial Token with RefreshToken and DCA-expiry metadata but no AccessToken**.
The manager retains this partial credential in the secret store and reports
`key_pending`, never connected. The UI offers an explicit completion retry that
reissues the key through Refresh without consuming the device grant again.
No automatic mint retries are made because a failed response can have an
unknown server-side outcome.

## Native inference contract and capability limits

| Driver | Registered BaseURL | Ferry protocol | Native request |
| --- | --- | --- | --- |
| xAI | `https://api.x.ai/v1` | `openai-responses` | `POST /v1/responses`, Bearer OAuth access token |
| Meta | `https://api.meta.ai/v1` | `openai-responses` | `POST /v1/responses`, Bearer minted API key |

Inference uses `Content-Type: application/json` and `Accept: application/json`
or `text/event-stream` for streaming. Authentication success alone does not
prove an account can use these inference endpoints, models, tools, media,
reasoning, streaming or subscription quota. Chat Completions and Anthropic
Messages are not declared native wire contracts by these drivers; existing
Ferry ingress conversion policy remains separate. Catalog reads attempt the
registered root `/models`; a returned model is not capability evidence. No
WebSocket transport or new media implementation is included.

The observed xAI executor defaults subscription HTTP traffic to the provider's
`https://cli-chat-proxy.grok.com/v1` service, with `X-XAI-Token-Auth: xai-grok-cli`,
`x-grok-client-identifier: grok-shell`, `x-authenticateresponse:
authenticate-response`, a client version and a Grok workspace User-Agent.
Its direct API branch uses Bearer auth and JSON/SSE headers without those CLI
identity headers. This implementation declares only the direct API BaseURL;
subscription-only access is not promised and no CLI-proxy route or client
impersonation is added.

The observed Meta executor sends `X-Client-Id: tbh:tui` and a Muse User-Agent
in addition to Bearer/JSON/SSE headers. These are observed client-specific
headers, not independently verified universal API requirements. This driver
uses an honest `Ferry/nativeauth` User-Agent for authentication, and does
not synthesize Muse identity. Runtime inference uses `Ferry/native` with
Bearer and the existing Responses caller headers. If the service
requires that identity, compatibility remains unsupported pending a legitimate
Ferry client integration. Meta `/responses/compact` is explicitly unsupported
by the observed executor; do not enable it based on successful authentication.

## HTTP boundary and validation

Only the supplied http.Client is used. A shallow copy retains its transport and
timeout, disables cookie-jar use, and rejects every redirect without modifying
the caller. No proxy configuration, environment endpoint override, fingerprint,
or fallback client is created. The supplied transport itself remains a trusted
caller dependency. Each request has an additional 30-second context deadline.
Responses are read through a 1 MiB cap and closed on all completed HTTP paths.
Errors contain only fixed messages, known sentinel errors, context cancellation,
or numeric HTTP status; no response body, URL, transport error text, token,
provider error_description, or parser excerpt is returned.

Offline tests use fake RoundTrippers only, covering device requests, state
preservation, pending/slowdown, refresh rotation, endpoint tampering, redirects,
response limits, secret-safe errors, cancellation, and minted-key/DCA lifetimes.
Run `go test ./internal/nativeauth`, `go test -race ./internal/nativeauth`,
and `go build ./internal/nativeauth` for local module validation.

The manager serializes refreshes and persists rotating-refresh intent before I/O.
Interrupted or failed rotating refresh returns durable `relogin_required`; the UI
offers a new login rather than replaying an uncertain refresh token.
