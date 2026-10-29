# Ferry Console v3 — Front-end API Contract

## Goal

Keep page code independent from backend storage/domain internals. Pages consume stable BFF-style ViewModels through `FerryAdapter`; `MockFerryAdapter` and `HttpFerryAdapter` implement the same surface.

The HTTP paths below are the implemented production-console contract. The Go management facade returns product-level ViewModels backed by the runtime snapshot, SQLite control plane and Ledger; `HttpFerryAdapter` translates those responses into the reviewed page ViewModels.

Only the unversioned `/api/*` surface is a product management API. `/api/v2/*` has no compatibility alias, `/api/auth/*` is reserved for the browser administrator session, and every retired legacy-only business path returns `404`. Management Tokens may use product resources but can never manage their own lifecycle; creation, high-risk enablement, rotation, revocation, and deletion require the Ferry Console administrator session.

## Adapter surface

| Method | Target HTTP | UI use |
| --- | --- | --- |
| `getAuthStatus()` / `initializeAdmin()` / `login()` / `logout()` / `changePassword()` | `/api/auth/*` | Browser-only administrator lifecycle |
| `getSecurityEvents()` | `GET /api/security-events` | Login, token, and credential audit |
| `getManagementTokens()` | `GET /api/management-tokens` | Safe token metadata |
| `createManagementToken(input)` | `POST /api/management-tokens` | Create + one-time bearer value |
| `updateManagementToken(id,patch)` | `PATCH /api/management-tokens/{id}` | Enable/disable, expiry, metadata, and per-token high-risk gate |
| `rotateManagementToken(id)` | `POST /api/management-tokens/{id}/rotate` | Rotate + one-time bearer value |
| `revokeManagementToken(id)` / `deleteManagementToken(id)` | `POST .../revoke` / `DELETE /api/management-tokens/{id}` | Irreversible revoke, then metadata deletion |
| `getOverview()` | `GET /api/overview` | Overview client experience, runtime status, usage and recent events |
| `getModels()` | `GET /api/models` | Models list |
| `getModel(id)` | `GET /api/models/{id}` | Model Workspace |
| `createOverride(modelId,input)` | `POST /api/models/{id}/overrides` | Create an ordered first-match request override |
| `updateOverride(modelId,overrideId,input)` | `PATCH /api/models/{id}/overrides/{override_id}` | Update or toggle an override through the revisioned draft flow |
| `updateModel(id,{overrides})` | `PATCH /api/models/{id}` | Atomically replace the ordered Override list when reordering |
| `deleteOverride(modelId,overrideId)` | `DELETE /api/models/{id}/overrides/{override_id}` | Remove an override through the revisioned draft flow |
| `simulateModel(id,input)` | `POST /api/models/{id}/simulate` | Planner preview without an upstream request |
| `reviewRouting(id,draft)` | `POST /api/models/{id}/routing/review` | Before/after + impact preview |
| `applyRouting(id,review)` | `POST /api/models/{id}/routing/apply` | Commit new revision |
| `getProviders()` | `GET /api/providers` | Provider list |
| `getProvider(id)` | `GET /api/providers/{id}` | Provider Workspace |
| `discoverProviderModels(input)` | `POST /api/providers/discover-models` | Fetch the upstream catalog before Provider creation |
| `discoverProviderProtocols(input)` | `POST /api/providers/discover-protocols` | Read cached per-model capability evidence by default and minimally probe only missing or explicitly refreshed evidence; real probes may be billable |
| `addProvider(input)` | `POST /api/providers` | Provider wizard |
| `saveProviderProtocolConfig(id,input)` | `PUT /api/providers/{id}/protocol-config` | Atomically replace one custom provider's shared endpoint, credentials, native model groups and conversion policy |
| `addConnection(providerId,input)` | `POST /api/providers/{id}/connections` | Connection wizard |
| `updateConnection(providerId,connectionId,patch)` | `PATCH /api/providers/{id}/connections/{connectionId}` | Connection actions |
| `deleteConnection(providerId,connectionId)` | `DELETE /api/providers/{id}/connections/{connectionId}` | Safe draft deletion |
| `testConnection(providerId,connectionId)` | `POST /api/providers/{id}/connections/{connectionId}/test` | Live validation request |
| `rotateConnectionSecret(providerId,connectionId,secret)` | `POST .../rotate-secret` | High-risk credential replacement |
| `getRequests(filters)` | `GET /api/requests?...` | Server-filtered request list |
| `getRequest(id)` | `GET /api/requests/{id}` | Execution Trace |
| `getApiKeys()` | `GET /api/api-keys` | API Key list |
| `getApiKey(id)` | `GET /api/api-keys/{id}` | Identity, secret metadata, usage and recent requests |
| `createApiKey(input)` | `POST /api/api-keys` | Create + plaintext response |
| `revealApiKey(id)` | `GET /api/api-keys/{id}/secret` | Explicit administrator reveal of the active secret |
| `updateApiKey(id,patch)` | `PATCH /api/api-keys/{id}` | Enable/disable identity |
| `rotateApiKey(id)` | `POST /api/api-keys/{id}/rotate` | Rotate + plaintext response; the active value remains revealable |
| `revokeApiKey(id)` | `POST /api/api-keys/{id}/revoke` | Immediate revocation |
| `deleteApiKey(id)` | `DELETE /api/api-keys/{id}` | Delete disabled identity metadata |
| `startConnectionOAuth(...)` | `POST .../oauth/login` | Reuse Codex OAuth authorization |
| `completeConnectionOAuth(...)` | `POST .../oauth/callback` | Complete pasted callback URL |
| `importConnectionOAuth(...)` | `POST .../oauth/import` | Import official Codex or CLIProxyAPI auth JSON without retaining the raw file |
| `getConnectionOAuthUsage(..., { refresh })` | `GET .../oauth/usage?refresh=...` | List, detail and editor share account-scoped cached snapshots; entering each surface reads cache first, then refreshes upstream. Concurrent refreshes coalesce; cached values stay visible with a border marquee while loading |
| `resetCooldowns(scope)` | `POST /api/runtime/clear-cooldown` or `POST /api/models/{model_id}/clear-cooldown` | Empty scope resets all; `provider_id`, `connection_id`, `upstream_model` intersect. Logical-model scope selects its routed supply. Successful resets are immediately persisted. |
| `getSettings()` / `updateSettings(patch)` | `GET/PATCH /api/settings` | Runtime settings; writes are high-risk |
| `getOrganizations()` | `GET /api/organizations` | Single-runtime scope boundary |
| `getRevisions()` | `GET /api/revisions` | Audit/revision history |
| `getRevision(id)` / `restoreRevision(id)` | `GET /api/revisions/{id}` / `POST .../restore-draft` | Inspect history and create a reviewable restore draft |
| `getAlerts()` / `acknowledgeAlert(id)` | `GET /api/alerts` / `POST .../acknowledge` | Persistent local alert lifecycle |
| `getBilling()` | `GET /api/usage-billing` | Usage & billing |

### Management Tokens

Cooldown reset is available in the Providers list (global or one Provider), Provider detail, connection drawer (connection or upstream model), and logical model detail. A model reset clears its shared connection gates while preserving sibling model cooldowns. It does not change configured enablement or durable usage accounting. Fresh upstream observations may limit requests again; cached usage reads do not change scheduling.

Token tables expose only id, name, description, preview, enabled/high-risk flags, expiry, last use, revocation, and timestamps. A raw `smt_...` value appears only in create/rotate responses with `Cache-Control: no-store`; the UI presents it in a one-time copy dialog. The high-risk switch is an administrator-controlled binary gate for apply/activation, secrets and credentials, inference API-key lifecycle, OAuth writes, settings writes, cooldown clearing, and quota overrides. Token requests that cross that gate receive `403 HIGH_RISK_NOT_ALLOWED` and must not be retried automatically.

## Core ViewModel semantics

### Overview

Must present client experience first while keeping Ferry service availability independent from historical
traffic and configuration health. At minimum return:

- client success / direct success / recovered success / failed
- active conversations, request rate, token rate, median latency
- request/token/cost totals
- token-weighted prompt-cache hit rates overall, by API key, and by logical model
- gap-filled request-volume trends for all calls, logical models, actual Providers, and actual Provider/upstream-model pairs; every series carries succeeded/failed counts so the UI can show its success rate
- recent runtime events, rendered after the usage and traffic sections

`cache_hit_summary` uses the protocol of the final upstream attempt, rather than the incoming client protocol.
For OpenAI-compatible usage, cached tokens are a subset of `input_tokens`; for Anthropic usage, cache read and
write tokens are added to `input_tokens` to form the eligible-input denominator. Rates are omitted when no
input usage was observed. API-key and logical-model rates are weighted by tokens, not averaged per request.

`traffic_trend` is emitted once at the finest product dimension (logical model + final Provider + actual upstream
model). The adapter aggregates those route series for the logical-model and Provider charts. A request that fails
before upstream selection remains in the all-calls and logical-model series, but is not attributed to a Provider.
Legacy rows without `logical_model_id` fall back to the client alias/requested model, and rows without
`upstream_model` fall back to the selected model; a missing Provider is never inferred.

The response also exposes independent status boundaries under `status`:

- `overall`: the runtime/API availability headline; a successfully serving management runtime reports `available`
- `client_traffic`: `no_traffic | healthy | degraded`
- `runtime_api`: management/runtime API reachability; currently `available` when this response can be served
- `configuration`: logical-model routing health

`overall` is deliberately not an aggregate severity. Selected-window request failures and unavailable model
routes must not relabel a reachable Ferry runtime as globally degraded. The UI must not use logical-model
configuration failure as evidence that successful client traffic or the management API is unavailable.
Recovered requests are successful client outcomes; only failed outcomes make client traffic degraded.

The v3 Overview renders Client Experience, Quota / Usage / Protection Cost, Prompt Cache Hit Rate, Traffic
Trends, and Recent Runtime Events in that order, with Recent Runtime Events last. It does not duplicate
`Needs Attention`, `Managed Automatically`, `Model Protection`, or `Operational Boundaries` as standalone
cards; model, provider, request and alert workspaces remain the drill-down surfaces for those facts.

Do not infer unknown measurements as `0`. Ledger aggregates expose
`tokens_observed` and `cost_observed`; the adapter renders `—` when the
corresponding measurement was not observed, even though SQLite numeric
columns use zero as their storage default.

### Model Workspace

Return:

- model identity, configured state `Active | Disabled`, and independent runtime status `Healthy | Protected | Degraded | Unavailable`
- ordered provider route
- upstream route availability rendered separately as `Route available | Route degraded | Route unavailable`
- live serving percentage per provider
- average time to first token and average end-to-end completion time for successful requests in the selected window
- recovered/fallback/client-visible failure counts
- overrides rendered in user language

Connection-level items must not be flattened into the provider route.

Protocol-scoped Connections may override the Provider endpoint with either
`base_url` or `full_url`, never both. `base_url` receives the standard protocol
path suffix; `full_url` is used exactly by capability discovery, Connection
Test, and routed traffic. Capability discovery records evidence for every
capability used by the current scheduler rather than inferring advanced support
from a successful text request or from an upstream model name.
Logical-model health and upstream-route availability are different dimensions and must not reuse the same
badge label even when both are derived from the same runtime snapshot.

Simulation is independent from the configured Active/Disabled state. For a disabled model with a valid saved
route, the server compiles an unpublished preview snapshot with that model temporarily enabled, runs the normal
planner, and returns `previewing_disabled_model:true`; it never changes the active revision or admits client
traffic. A disabled model without a valid route returns a specific `422` configuration error.

The simulation input accepts `caller_id`, `protocol`, `stream`, RFC3339 `at`, and
`required_capabilities` (including minimum context/output sizes). Its response identifies the matched Override,
default and effective routes, ranked connection candidates with exclusion reasons, expected selection and any
planning failure. A planning failure with candidate evidence is a successful simulation response, not a generic
transport error.

Overrides are evaluated in saved order and the first enabled match wins. Match fields support callers, protocols,
weekdays, an IANA timezone plus time window, and capability requirements. Actions either adjust the default route
with explicit prefer/avoid steps or replace it with a custom route. Reordering is a real revisioned model update;
the backend rejects an enabled Override shadowed by an earlier broader match.

### Provider Workspace

Return:

- provider identity and provider-level status: `Healthy | Recovering | Exhausted | Unavailable`
- capacity/traffic
- connection list with role/status: `Serving | Standby | Cooling | Unavailable`
- last issue / cooldown / conversation bindings where observable
- last successful provider request time from the ledger
- editable shared endpoint and key, per-protocol endpoint/key overrides, native model groups, automatic-conversion policy, enabled state, and optional store-backed key replacement

Recovery-policy controls and the provider-local recovery-event table are intentionally not exposed until those
workflows are complete. Request-level recovery evidence remains available from the Requests page.

Provider creation and editing discover models through the authenticated management backend rather than from
the browser. Protocol discovery reads the shared `/models` catalog, then resolves capability evidence for each
explicitly configured model and native protocol; it never probes the whole catalog and verifies at most 20
configured models per protocol in one request. Unproven models remain available for explicit assignment and
Ferry conversion. The temporary secret may be used for discovery but is never returned or logged.

Capability evidence is keyed by Provider identity, an opaque SHA-256 endpoint fingerprint, upstream model,
native protocol and capability. Raw endpoint URLs and credentials are not stored in the evidence table. For a
new Provider that has not yet been saved, the backend derives a stable temporary Provider identity from the
endpoint fingerprint so repeated wizard requests can still reuse evidence. Passing the saved `provider_id`
preserves evidence across later edits; changing the effective endpoint changes the fingerprint and causes only
that endpoint's evidence to be reprobed.

The current scheduler capability set is `text`, `images`, `stream`, `tools`, `parallel_tools`, `reasoning`,
`structured_output`, `vision`, `context_window`, and `max_output`. OpenAI Chat and Responses combine text,
stream, structured-output, reasoning and vision checks into one probe when possible; a second probe combines
tools and parallel tools. Anthropic uses the equivalent text/stream/thinking/vision probe plus the tool probe,
while `structured_output` is reported as unsupported by the native protocol contract. The Images protocol probes
`images` independently. A normal full-capability text model therefore requires two inference requests, and a
complete cache hit requires none. A deterministic combined-probe rejection may be split into focused probes to
identify the rejected capability; transient timeouts, connection failures, rate limits and upstream 5xx responses
remain `unknown`, are not negatively cached, and do not trigger an expensive split.

Parallel-tools evidence requires observed behavior, not merely an accepted request parameter: the model must
return two different, complete function calls with valid JSON-object arguments in the same assistant turn. If an
upstream silently ignores the parallel request and returns only one call, Ferry records `tools` as supported
and `parallel_tools` as unsupported after the single-tool fallback.

The evidence registry is contract-tested against every field in `domain.Capabilities`, so adding a scheduler
capability cannot silently bypass discovery. Sampling controls, `tool_choice`, prompt-cache accounting and
conversation continuity are protocol or runtime semantics rather than independent scheduler capability bits.
Audio, embeddings, video, hosted search, file search and computer use are not currently advertised as discovered
capabilities because Ferry has no corresponding data-plane protocol and scheduler contract. Adding one of
these later requires the domain capability, evidence registry, probe strategy, Console declaration UI and
contract tests to be extended together.

Custom Provider capability declarations are never expanded from GLM, Kimi, or other model-family names. Codex
OAuth remains the deliberate exception because Ferry owns its model catalog and wire-protocol contract.

Supported evidence is fresh for 30 days and deterministic unsupported evidence for 7 days. `refresh:false` (the
normal Console path and default API behavior) reuses all fresh evidence; `refresh:true` is the only way to force a
new probe. `context_window` and `max_output` are not discovered by costly boundary searches and are returned as
`unknown` with source `authoritative-config-required` until supplied by authoritative configuration. The Console
writes back only `supported` or `unsupported` observations; `unknown` never erases an explicit declaration.

`priority` (Fast) is an account/endpoint declaration, not a functional probe result. Chat/Responses discovery
returns `unknown` with source `authoritative-config-required`; other protocols report `unsupported` from
`protocol-contract`. Codex catalog sync reads explicit `service_tiers` for both new and existing models,
and static presets preserve this account-specific bit. Explicit lists without priority revoke it; absent/null
metadata preserves the prior declaration. The Console exposes it as a capability and an override predicate.
OpenAI `service_tier=priority` and its `fast` request alias require this capability; accepting a parameter or
responding quickly is not evidence of support.

Each `model_results` item returns the normalized `capabilities`, one `capability_evidence` record per relevant
capability, `cache_hit`, the number of real `probe_requests`, and an optional `evidence_error`. Evidence status is
`supported | unsupported | unknown`; source is `probe`, `cache`, `protocol-contract`,
`capability-prerequisite`, or `authoritative-config-required`. Opening the Provider editor does not automatically
start discovery or incur inference traffic.

Each protocol can override the shared Base URL, key and native model set. `protocol_conversion` is independent
per client protocol: a native connection always accepts its own protocol, while non-native requests may use
Ferry conversion only when the corresponding switch is enabled. When both routes exist, scheduling prefers
the native connection and keeps converted connections as fallback. A missing `protocol_conversion` map retains
the legacy behavior of allowing all conversions.

The preference is evaluated for every request protocol, not assigned globally to one connection. For example,
when the same upstream model is native on Chat, Responses and Anthropic, an Anthropic ingress request uses the
Anthropic-native connection while a Chat ingress request uses the Chat-native connection. Other native protocol
connections remain lower-ranked conversion fallbacks. The console groups those same-name fallbacks by model and
shows all of their source protocols without merging their endpoint, credential or capability records.

### Request Detail

The top-level entity is one **client request**. Return its:

- request summary
- routing decision
- ordered internal attempts
- connection rebinds
- provider fallbacks
- conversation migration/rebind facts when available
- final response/failure
- recovery overhead
- response mode (`Streaming | Non-streaming`)
- time to first token for streaming responses (`ttft_ms`; render `—` when not observed or non-streaming)
- total end-to-end duration (`duration_ms`)
- token usage (`input_tokens`, `output_tokens`, and `reasoning_tokens`)
- prompt-cache usage (`cache_read_tokens` and `cache_write_tokens`)

Request lists expose the same response mode, TTFT, total-duration, token, and cache fields as detail. Provider
and actual upstream model are rendered together because they identify one selected upstream route target.
Attempt detail repeats token, cache, first-frame, and completion measurements for the concrete upstream call.
Request detail renders the raw provider read/write token counters. Overview hit rates must use the server's
protocol-normalized `cache_hit_summary` and must not be recalculated from mixed-protocol raw totals in the UI.

Execution Trace events must be sorted by their recorded timestamps. Fallback traces derive the completion
timestamp from request start plus duration so the UI cannot show a response completing before an attempt.
Pending request details expose the latest in-memory `execution_trace_events` before completion. Actual
body receive/parse, planning and compatibility phase boundaries identify pre-upstream waits;
`ADMISSION_COMPLETED` and `UPSTREAM_FIRST_EVENT` identify queue and upstream waits within an attempt.
Phase durations pair their recorded start/end timestamps (and attempt number for admission/upstream
events); missing starts render `—`. Only `COMPLETE` uses the full request duration. A client cancellation
before an attempt starts must not create a synthetic zero-duration attempt.
Route traces distinguish logical-model resolution, route-plan resolution, candidate exclusion and final
connection selection. Resource events expose the public `connection_id` and upstream `protocol`; affinity
events additionally expose `affinity_scope` (`CONVERSATION | CACHE | CALLER`) and the non-secret
`affinity_source`, so cache or caller stickiness is never presented as conversation continuity.

Do not expose internal attempts as separate client requests.

### Routing review/apply

`review` must be side-effect-free with respect to the active runtime. It creates a durable open draft and returns:

- `before`
- `after`
- estimated affected traffic
- active conversation count
- conversation strategy for existing/new conversations
- warnings/validation issues
- expected next revision / CAS token

The console submits one ordered list of `provider_id + upstream_model + recover_on` steps. The same Provider may
appear more than once when the actual upstream model differs. Each non-final step explicitly selects which
retryable upstream failures advance to the next step; the final step has no progression triggers. Client request
errors never advance the route, and connection retry inside one Provider remains automatic. Pre-commit lossless
protocol-conversion failures and parameters unsupported only by the current target automatically try the next route;
they are compatibility facts rather than configurable upstream recovery. The UI exposes the remaining typed
`recover_on` controls directly instead of separate overlapping Fallback or Protection switches. For backwards
compatibility only, an omitted `recover_on` field receives the standard operational set; an explicit list—including
`[]`—is authoritative for those failures. Validation failures return every concrete problem under
`error.details`, and `error.message` includes the first problem so clients never have to display only a generic
“review failed” message.

`apply` should use optimistic concurrency. A stale revision/version should return `409` with enough information for the UI to refresh and review the new diff before retrying.

### API keys

List responses contain only masked/previews. Plaintext appears only in the explicit create/rotate/reveal response required by product policy and must never be placed in logs, default tables or screenshots.

The console labels model/protocol scope, expiry, request rate, and monthly spend as **Access & Limits**. These
settings constrain one client credential and are not routing policies.

Detail responses return secret-level `last_used_at` when recorded and a key-level `last_request_at` derived
from the newest ledger request. The UI may use `last_request_at` only as an explicitly labelled key-level
fallback; it must not present that value as exact activity for an individual historical secret.

## Error contract

Recommended shape:

```json
{
  "error": {
    "code": "REVISION_CONFLICT",
    "message": "Routing changed since this review was created.",
    "current_revision": 186,
    "retryable": true
  }
}
```

The UI needs distinguishable handling for at least:

- `400` malformed request
- `401` missing/invalid administrator Session or Management Token
- `403` CSRF rejected, administrator Session required, or per-token high-risk gate disabled
- `404` resource missing
- `409` revision/version conflict
- `422` business validation failure
- `428` missing optimistic-concurrency precondition
- `503` dependent capability unavailable

## Organization scope

`GET /api/organizations` currently returns one `Local Ferry` scope and `organization_scope_supported:false`. The real adapter renders that boundary and disables synthetic member/tenant claims. If isolation is implemented later, scope every relevant read/write consistently via authenticated context or a standard URL/path/query mechanism; do not add proprietary inference-request headers to public OpenAI/Anthropic data-plane protocols.

### Model creation and request costs

Settings exposes integer `auto_logical_model_limit` (0–10000, default 10). It is stored in the versioned configuration. The limit applies only to aggregators whose Provider or Connection endpoint host is listed in `internal/config/logical_model_policy.json` (exact `openrouter.ai` by default). It counts distinct upstream models across that provider's complete connection catalog, including already listed models. Above the limit, new same-name Logical Models require explicit selection; zero requires manual selection for listed aggregators. Other providers, including Codex, automatically create all missing same-name Logical Models, including previously synchronized catalog entries. Changing the host allowlist requires a rebuild. Existing manual routes are preserved; existing generated routes continue reconciliation. Raising the limit can create models immediately when saving. The same policy applies to create/edit, discovery, manual and scheduled synchronization, and restored configurations.

Request summaries expose USD `total_cost`, `cost_observed`, `estimated_cash_cost`, `allocated_sub_cost`, `serving_cost`, `recovery_overhead_cost` and `shadow_cost`. Total matches the ledger: serving + recovery when either is nonzero, otherwise legacy estimated cash. A zero record is not proof of free usage; absent observed pricing renders unavailable. Attempt details include `rate_card_version`, `plan_kind`, `multiplier`, `quota_units` and recorded monetary components. Historical unit rates are not retained and must not be reconstructed using current rates.


### Live requests, Queue and concurrency

GET /api/requests and request details include requests from inference-handler entry, before body parsing and admission. Nonterminal statuses are received, waiting, active, recovering and streaming; these must not render as success or failure. The request ID stays stable through finalization. Live records persist asynchronously and are excluded from finalized billing/analytics. Stranded records are finalized as canceled after restart. Requests and Queue refresh every three seconds while visible.

GET /api/queue returns generated_at, open_requests, aggregate waiting and running, normalized global defaults, connections, and active scheduler requests. open_requests counts inference HTTP handlers from entry until return, including time spent writing a stream after the upstream slot is released. running is upstream admission occupancy; it can be zero while open_requests is positive. Each connection includes connection_id, provider_id, identity, enabled, policy_source, optional explicit concurrency, and runtime. Runtime fields are in_flight, waiting, max_concurrent, max_queue, max_wait_seconds, queue_when_full, queue_on_throttle, cooling_until, and recovering. Gate counts include background jobs sharing capacity; the request list includes client traffic only. Configured connections without execution targets are also listed with zero activity.

Concurrency settings apply per connection, with precedence Connection > Provider > global defaults. GET /api/settings exposes concurrency and revision. To change global defaults, PATCH /api/settings requires concurrency and expected_revision; stale revisions return 409. Existing high-risk settings authorization applies. Queue edits global and Connection settings; Provider overrides remain in the Provider editor. Connection edits use the existing adapter and draft/apply flow; explicit concurrency: null restores inheritance.

max_concurrent: 0 means unlimited, max_queue: 0 means the built-in 256, and max_wait_seconds: 0 means the built-in 120 seconds (maximum 1800). Omitted queue behavior flags default to true. A global empty object restores built-in defaults. Changes update admission without interrupting running requests; existing waiters retain their deadlines. The Queue editor retains unsaved fields while runtime tables refresh.


### Codex version automation

Settings → General → Codex Client Version publishes manual version, automatic
update switch, and stable/prerelease channel together via draft → validate →
revision-CAS apply. GET /api/settings exposes codex_client_version,
codex_auto_update (default false), codex_release_channel (stable/prerelease),
codex_auto_version, codex_effective_client_version, codex_default_client_version,
and codex_update_status (status, last_checked_at, next_run_at, latest_version,
last_error). Update intent fields are codex_client_version, codex_auto_update and
codex_release_channel; codex_auto_version is server-managed. These fields are not
direct settings PATCH inputs.

Manual nonempty values suspend the automatic check and take precedence. Clearing
the manual pin with automatic updates enabled retains the effective version until
a newer release arrives. Disabling updates retains the resolved automatic version.
Release checks run hourly, never downgrade, and retain the active version on
failure. The switch is opt-in, and existing model-sync switches, custom routes and
logical-model limits remain effective. Unsaved settings are sent only after Review
& Publish; status reflects the published configuration.

### Automatic Codex reset cards

`GET/PATCH /api/settings` exposes `codex_usage_reset_auto` as the complete object
`{"enabled":true,"threshold_minutes":20}`. The default is enabled with 20 minutes;
valid thresholds are integers from 1 to 1440. Settings → General edits both fields
through the existing high-risk settings API. Settings persist in SQLite.

The background scan runs at startup and every 30 seconds for enabled Codex OAuth
providers/connections. Available cards qualify only when remaining validity is
positive and no greater than the threshold. It selects the earliest expiry,
uses the existing durable reset journal, and rechecks identity, policy, and target
before dispatch. Missing/invalid expirations are skipped. Manual pending intents
and uncertain results block automation; submitted automatic attempts are not
replayed. Manual confirmation remains administrator-session-only.

## Credential migration

`/import.html` uses the same-origin HTTP adapter and authenticated management session.
`POST /api/imports/preview` accepts `source`, `namespace`, `document`, optional
`auth_files` and `client_keys`. `POST /api/imports/apply` additionally requires explicit
`selected` IDs. Both require high-risk permission for management tokens; preview is
read-only. Responses never include credential material. Apply returns per-item results,
with HTTP 207 for partial failures; created providers, connections and client keys are
disabled. Existing IDs are not overwritten. See [import contract and supported source
formats](../../docs/guide/credential-import.md) for retry semantics and limitations.
