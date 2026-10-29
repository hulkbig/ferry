# Native Antigravity implementation notes

Recorded 2026-10-23. Local implementation and offline verification only; no live provider request, credentials, deployment, or product availability claim.

## Integrated runtime and UI contract

- The default manager registers `nativeauth.NewAntigravityDriver() Driver`. Driver ID: `antigravity`; protocol: `openai-chat`; `Info.BaseURL`: `https://daily-cloudcode-pa.googleapis.com`.
- The authenticated native transport resolves the current token, then sends the synthetic Chat request through `nativewire.NewAntigravityTransport(base, token.Extra["project_id"])`. It constructs the wire transport from that same account/token on each request. The inference request cannot select a different project. `internal/egress/native_chat.go` validates conversion during route preparation, and the Provider editor supports the browser login flow.
- Synthetic endpoint is exactly `POST <Info.BaseURL>/chat/completions`. Only a bearer access credential is consumed. Host, method, path, escaped path, userinfo, query, fragment, project and model are validated; arbitrary headers/cookies/keys are not forwarded. Redirects are not followed. Native upstream endpoints are `:generateContent` and `:streamGenerateContent?alt=sse` under `/v1internal`.
- Login fields: `client_id` and `client_secret`, both required. They belong to an authorized operator-controlled OAuth client with access to the Cloud Code API. Register redirect `http://127.0.0.1:51121/oauth-callback`; the driver does not start a listener. The Provider editor accepts the full callback URL and submits it through the management completion API, clearing it on submission. Client ID and client secret are entered explicitly; no environment credentials are imported. Exact origin/path, state (one occurrence), code (one occurrence), session expiry and PKCE verifier are checked before exchange.
- Secret-store-only `Token.Extra` keys: `client_id`, `client_secret`, `project_id`. Session Extra contains the two client values; the public manager View omits Extra/verifier/token. Client secret is sent only in Google's token exchange/refresh form; it is never included in the consent URL, returned errors, or this document.
- Authorization requests add S256 PKCE, request offline consent, and use the five observed Google scopes: cloud-platform, userinfo.email, userinfo.profile, cclog, experimentsandconfigs. Refresh is bound to the same client, preserves refresh credentials when not rotated, and retains the discovered project. No userinfo identity lookup is necessary for the account binding.

## Source evidence and unresolved external prerequisites

Verified reference: [CLIProxyAPI `d946c4fba6fc9a5cfbca1f3d85184d62ad2fb683`](https://github.com/router-for-me/CLIProxyAPI/tree/d946c4fba6fc9a5cfbca1f3d85184d62ad2fb683), This is the inspected revision, not a claim about the latest upstream version.

Observed schemas came from:

- `internal/auth/antigravity/auth.go`: token forms, loadCodeAssist response project variants, allowed default tiers, onboardUser request/poll response.
- `internal/runtime/executor/antigravity_executor_auth.go` and `antigravity_executor_request.go`: account-specific project, envelope, endpoint selection and request/session IDs.
- `internal/translator/antigravity/openai/chat-completions/antigravity_openai_request.go` and `antigravity_openai_response.go`: Gemini contents/function parts, parametersJsonSchema, wrapped candidates and usage counts.
- `internal/runtime/executor/antigravity_executor_split_usage_test.go`: finish followed by an empty-text candidate plus a usage tail.

Implementation is independent Go standard-library code; no helper implementation was copied, no third-party dependency was added, and no source client secret was copied. No bridge process, fabricated device identity, signature bypass, impersonated version, or model-name capability inference is used. HTTP User-Agent identifies `Ferry/native-antigravity`. The envelope `userAgent: antigravity` and metadata IDE discriminator are observed protocol fields; no fake client/OS version is sent.

**External blocker:** the reference uses a product-bound OAuth client and does not demonstrate PKCE for this flow. An arbitrary new Google OAuth client is not established as entitled to these APIs. The operator must provide a permitted matching client and establish OAuth/Cloud Code entitlement. Neither successful offline fixtures nor generic Google consent establish this. Without authorized live verification this feature must remain unverified, not advertised as working production Antigravity support.

Project acquisition is part of explicit login completion: loadCodeAssist first; if no project is returned, only an explicitly default entitled tier may be onboarded. There is no guessed fallback tier or project. Onboarding polls at most five times, every two seconds, within a 45-second total discovery context (each HTTP operation at most 30 seconds). It can change provider-side state. Status reads never onboard or poll. Missing entitlement, invalid/missing project, provider errors and exhausted onboarding fail closed. An OAuth code may already have been consumed if discovery subsequently fails; start a new login rather than replaying the same callback.

## Exact supported conversion boundary

Implemented local wire contract: text strings/text-part arrays, initial system/developer instructions, user/model turns, function declarations with JSON schema, function choices, full function calls and paired results, temperature/top_p/max token/stop settings, one candidate, wrapped nonstream responses and SSE streams. Function call IDs are validated, unique within the conversation, and results must resolve a preceding outstanding call. Adjacent tool results become one Gemini user turn. A call cannot be left unresolved before generation.

Wrapped usage includes prompt, output plus thought tokens, cached tokens, and total. Streaming cumulative usage counts use the largest observed value per field, including the observed empty-text usage tail. Usage chunks obey `stream_options.include_usage`; finish is emitted once after the upstream stream ends, then optional usage and `[DONE]`. Truncation/malformed data/midstream errors emit a sanitized error and **no** success terminator. Closing/canceling the translated body closes upstream I/O. HTTP failures preserve 401/403/429/5xx and Retry-After; redirects become 502 and provider error bodies are not echoed.

**Explicitly unsupported, not silently approximated:** signed/stateful thinking or tool continuation, previous-response/session state, reasoning controls/replay, images/audio, multiple candidates, strict tool schemas, unsupported request extensions and unknown part types. Unknown request fields are rejected before any upstream call. An upstream thoughtSignature or unsupported output part fails conversion rather than stripping signatures and handing back an unusable continuation. Unsigned thought text may be returned as reasoning_content; this does not authorize signed reasoning replay. No claim of end-to-end compatibility is made for models that require signed tool history. The native Chat abstraction does not provide an account/model-bound signature replay contract; those modes remain unsupported.

## Verification

Offline tests use intercepting RoundTrippers and synthetic fixtures only, with no real provider I/O. They cover authorization URL secrecy/PKCE, callback/session binding, refresh retention, account project acquisition, entitlement failures, onboarding response errors, redirect refusal, sanitized token errors, native envelope and header isolation, function results, unsafe input rejection before network, text/tool SSE, split usage tails, missing finish, signature/unsupported output failure, HTTP status preservation and upstream cancellation.

Local checks cover `internal/nativeauth`, `internal/nativewire`, `internal/egress`, `internal/management`, and the console native OAuth tests. `go test -race ./internal/nativeauth ./internal/nativewire` exercises the driver and wire modules. Live OAuth consent, entitlement/onboarding, signed continuation and production rollout are not verified by these tests.

Model catalog discovery is not implemented for this driver. The editor defaults
model sync to `off`, requires explicit model IDs, and preserves capability probes.
