# Ferry management API

All paths below are relative to `FERRY_MANAGEMENT_URL`. Unless marked **Session only**, they require `Authorization: Bearer $FERRY_MANAGEMENT_TOKEN`. JSON is the default request and response format.

## Authentication and risk

`GET /api/access` returns the authenticated actor, token metadata, and `allow_high_risk`. Management Token identity endpoints (`/api/management-tokens*`) deliberately require a browser administrator session and are unavailable to this Skill.

Risk classes:

- **Ordinary:** reads; alert acknowledgement; drafts; validation; simulation; resource edits that only create or update a draft; provider discovery and connection tests. Discovery may send billable upstream probes unless its capability evidence cache is complete.
- **High-risk:** draft/routing apply; immediate provider model sync (publishes new models); secrets; settings writes; API-key secret/lifecycle operations; connection credential rotation; OAuth writes; cooldown clearing; quota overrides.

For high-risk calls, obtain explicit user approval and pass `--confirm-high-risk`. The server still rejects the request unless the administrator enabled the token's high-risk switch.

## Complete endpoint inventory

| Risk | Method and path | Purpose |
| --- | --- | --- |
| Ordinary | `GET /api/access` | Current Management Token identity and gate |
| Ordinary | `GET /api/security-events` | Recent authentication and credential audit events |
| High | `GET /api/secrets` | List safe secret metadata |
| High | `POST /api/secrets/{name}` | Store `{ "value": "..." }` |
| High | `DELETE /api/secrets/{name}` | Delete one stored upstream secret |
| Ordinary | `GET /api/analytics` | Reconciled model/provider spend and quality; optional model/provider intersection and aggregate window |
| Ordinary | `GET /api/overview` | Client outcomes, runtime status, rates, usage and recent events |
| Ordinary | `GET /api/organizations` | Single-runtime scope and isolation capability |
| Ordinary | `GET /api/usage-billing` | Usage/cost summary and breakdown; accepts an aggregate window |
| Ordinary | `GET /api/usage-billing/pricing` | Reference-price cache, source version, schedule and refresh status |
| Ordinary | `POST /api/usage-billing/pricing/refresh` | Refresh public reference prices; retained cache on failure; no bill or rate-card mutation |
| Ordinary | `GET /api/billing/plan-templates` | Built-in platform plan templates with materialized quota buckets |
| Ordinary | `GET /api/billing/pricing-sync` | Proposal-only rate cards from a public pricing catalog (`source=models.dev`) with a new/changed diff; never mutates config |
| Ordinary | `GET /api/alerts` | Persistent local alerts |
| Ordinary | `POST /api/alerts/{id}/acknowledge` | Acknowledge an alert |
| Ordinary | `GET /api/models` | Logical-model inventory; accepts `window` |
| Ordinary | `POST /api/models` | Create a disabled or routed model draft |
| Ordinary | `GET /api/models/{id}` | Model, effective routes, overrides, runtime evidence |
| Ordinary | `PATCH /api/models/{id}` | Update a model draft; optional `draft_id` query |
| Ordinary | `DELETE /api/models/{id}` | Delete in a draft; use `cascade=true` when acknowledged |
| Ordinary | `POST /api/models/{id}/overrides` | Create an ordered conditional rule in a draft |
| Ordinary | `PATCH /api/models/{id}/overrides/{override_id}` | Replace/update one conditional rule |
| Ordinary | `DELETE /api/models/{id}/overrides/{override_id}` | Remove one conditional rule |
| Ordinary | `POST /api/models/{id}/simulate` | Simulate active routing without upstream traffic |
| Ordinary | `POST /api/models/{id}/routing/review` | Build a revision-bound before/after review |
| High | `POST /api/models/{id}/routing/apply` | Apply a reviewed routing draft |
| Ordinary | `GET /api/providers` | Provider and connection inventory; accepts `window` |
| Ordinary | `POST /api/providers/discover-models` | Read/probe an upstream model catalog |
| Ordinary | `POST /api/providers/discover-protocols` | Resolve cached capability evidence and minimally probe missing or explicitly refreshed protocol/model pairs; probes may be billable |
| Ordinary | `POST /api/providers` | Create a provider draft |
| Ordinary | `GET /api/providers/{id}` | Provider, connections, runtime state, use sites |
| High | `POST /api/providers/{id}/models/sync` | Immediately read the model catalog and publish new channel/automatic logical models; no request body |
| Ordinary | `PATCH /api/providers/{id}` | Update a provider draft |
| Ordinary | `PUT /api/providers/{id}/protocol-config` | Atomically replace native protocol connections in a draft |
| Ordinary | `DELETE /api/providers/{id}` | Delete in a draft; use `cascade=true` when acknowledged |
| Ordinary | `POST /api/providers/{id}/connections` | Add a connection in a draft |
| Ordinary | `PATCH /api/providers/{id}/connections/{connection_id}` | Update a connection in a draft |
| Ordinary | `DELETE /api/providers/{id}/connections/{connection_id}` | Delete in a draft; accepts `cascade=true` |
| Ordinary | `POST /api/providers/{id}/connections/{connection_id}/test` | Real connection validation request |
| High | `POST /api/providers/{id}/connections/{connection_id}/rotate-secret` | Replace credential with `{ "secret": "..." }` |
| High | `POST /api/runtime/clear-cooldown` | Reset all runtime cooldowns, or intersect optional `provider_id`, `connection_id`, `upstream_model` JSON filters |
| High | `POST /api/providers/{id}/clear-cooldown` | Reset all runtime models under a Provider; optional `connection_id` / `upstream_model` filters |
| High | `POST /api/providers/{id}/connections/{connection_id}/clear-cooldown` | Reset connection models; optional `upstream_model` selects one model |
| High | `POST /api/models/{id}/clear-cooldown` | Reset supply for a logical model, including default and override routes |
| High | `POST /api/providers/{id}/connections/{connection_id}/set-quota` | Override one model quota |
| High | `POST /api/providers/{id}/connections/{connection_id}/oauth/login` | Start browser/device OAuth login |
| Ordinary | `GET /api/providers/{id}/connections/{connection_id}/oauth/status` | Redacted active/pending OAuth status |
| High | `DELETE /api/providers/{id}/connections/{connection_id}/oauth/login` | Cancel pending login |
| High | `DELETE /api/providers/{id}/connections/{connection_id}/oauth` | Disconnect account and refresh runtime |
| High | `POST /api/providers/{id}/connections/{connection_id}/oauth/callback` | Complete login from `code`+`state` or `callback_url` |
| High | `POST /api/providers/{id}/connections/{connection_id}/oauth/import` | Import bounded Codex `auth.json` as `auth_json` |
| Ordinary | `GET /api/providers/{id}/connections/{connection_id}/oauth/usage` | Subscription snapshot; `refresh=false` is cache-only |
| Ordinary | `GET /api/oauth/native/drivers` | Native driver inventory; no account/model capability claim |
| Ordinary | `GET /api/oauth/native/{driver}/accounts/{account}/status` | Redacted local account status; no provider polling |
| High | `POST /api/oauth/native/{driver}/accounts/{account}/login` | Start native browser/device authorization or service-account import |
| High | `POST /api/oauth/native/{driver}/accounts/{account}/complete` | Explicit callback or device completion; refresh runtime on success |
| High | `DELETE /api/oauth/native/{driver}/accounts/{account}/login` | Disconnect native credentials and pending login; refresh runtime |
| Ordinary | `GET /api/oauth/usage-reset-requests` | Local pending/outcome journal; never queries or consumes upstream credits |
| High | `POST /api/providers/{id}/connections/{connection_id}/oauth/usage-reset-requests` | Prepare a selected Codex credit for browser confirmation; reads upstream only |
| High | `POST /api/providers/{id}/connections/{connection_id}/oauth/usage-reset-requests/{request_id}/cancel` | Cancel an unsubmitted intent |
| Session only | `POST /api/providers/{id}/connections/{connection_id}/oauth/usage-reset-requests/{request_id}/confirm` | Administrator session + CSRF + explicit popup confirmation; unavailable to this Skill |
| Ordinary | `GET /api/requests` | Server-filtered request ledger |
| Ordinary | `GET /api/requests/{id}` | Client request, attempts, trace, impact, debug capture |
| Ordinary | `GET /api/queue` | Live admission counts, per-connection capacity and effective concurrency policy |
| Ordinary | `GET /api/settings` | Runtime-local settings |
| High | `PATCH /api/settings` | Change runtime-local settings |
| Ordinary | `GET /api/api-keys` | Safe API-key identity and secret metadata |
| High | `POST /api/api-keys` | Create identity and return one-time secret |
| Ordinary | `GET /api/api-keys/{id}` | Identity, usage, secret metadata, recent requests |
| High | `GET /api/api-keys/{id}/secret` | Explicitly reveal active secret |
| High | `PATCH /api/api-keys/{id}` | Enable/disable or change access/limits |
| High | `DELETE /api/api-keys/{id}` | Delete eligible identity metadata |
| High | `POST /api/api-keys/{id}/rotate` | Rotate and return one-time secret |
| High | `POST /api/api-keys/{id}/revoke` | Permanently revoke identity and its secrets |
| High | `POST /api/api-keys/{id}/secrets/{secret_id}/retire` | Retire a grace secret |
| Ordinary | `POST /api/drafts` | Create a draft from the active revision |
| Ordinary | `GET /api/drafts/{id}` | Read draft content and base revision |
| Ordinary | `PATCH /api/drafts/{id}` | Replace/update draft content |
| Ordinary | `DELETE /api/drafts/{id}` | Discard a draft |
| Ordinary | `POST /api/drafts/{id}/validate` | Validate complete draft |
| Ordinary | `POST /api/drafts/{id}/simulate` | Simulate a draft; accepts `model` query |
| High | `POST /api/drafts/{id}/apply` | CAS apply with `expected_revision` |
| Ordinary | `GET /api/revisions` | Immutable configuration revisions |
| Ordinary | `GET /api/revisions/{revision}` | Revision metadata and redacted config/diff |
| Ordinary | `POST /api/revisions/{revision}/restore-draft` | Create a reviewable draft from history |

## Common request shapes

Create a logical model (safe initial state):

```json
{"id":"smart","display_name":"Smart","description":"Client model","enabled":false,"default_route":{"steps":[]}}
```

Logical models accept `first_output_timeout_seconds` in create, PATCH, draft
`intent.logical_models`, and routing review payloads. It is an integer: `0`
disables this timeout (also the behavior of older configurations that omit it),
and `1..7200` enables it for that model. Negative, fractional, and larger values
are rejected. PATCH and routing review preserve the current value when omitted;
send `{"first_output_timeout_seconds":0}` explicitly to disable it. Model details
and draft/revision intent readback include the field even when it is `0`.

The limit starts after each attempt acquires concurrency capacity and ends at its
first effective text, reasoning, or tool output. It also applies when a
non-streaming Codex request consumes upstream SSE. Queuing does not consume this
limit, and a stream with output already in progress may continue normally.
Before client commitment, expiry uses the existing `ttft_timeout` route recovery
rules; it does not grant additional route permissions. There is no global
first-output timeout default, and adding this field does not enable existing models.

Route step and review:

```json
{
  "routes":[
    {"provider_id":"provider-a","upstream_model":"model-a","recover_on":["CONNECT","RATE_LIMIT","PROVIDER_5XX"]},
    {"provider_id":"provider-b","upstream_model":"model-b","recover_on":[]}
  ],
  "conversation_strategy":"keep-existing"
}
```

Apply only the exact returned review:

```json
{"draft_id":"draft-id-from-review","expected_revision":12}
```

Create a custom provider with one connection:

```json
{
  "id":"provider-a",
  "name":"Provider A",
  "type":"custom",
  "base_url":"https://provider.example/v1",
  "enabled":true,
  "connection":{
    "id":"provider-a-primary",
    "identity":"primary",
    "enabled":true,
    "credential":{"secret_ref":"store:provider-a"},
    "models":[{"upstream_model":"model-a","protocols":["openai-chat"],"capabilities":{"text":true,"stream":true}}]
  }
}
```

Discover native protocols and all capabilities used by routing:

```json
{
  "provider_id": "provider-a",
  "type": "custom",
  "base_url": "https://provider.example/v1",
  "credential_ref": "store:provider-a",
  "refresh": false,
  "protocols": [
    {"protocol": "openai-responses", "models": ["model-a"]}
  ]
}
```

The evidence key is Provider + opaque endpoint fingerprint + upstream model + native protocol + capability.
The server never persists the raw endpoint or credential in this cache. The covered routing capabilities are
`text`, `images`, `stream`, `tools`, `parallel_tools`, `reasoning`, `structured_output`, `vision`, `priority`,
`context_window`, and `max_output`. `reasoning` covers OpenAI reasoning and Anthropic thinking.

Fresh supported evidence is reused for 30 days and deterministic unsupported evidence for 7 days. Full cache
hits issue zero inference requests; set `refresh:true` only when the user explicitly wants to retest. Endpoint
changes naturally miss the cache. Transient timeout, connection, rate-limit, and upstream 5xx failures produce
`unknown` evidence and are not negatively cached. `context_window` and `max_output` are returned as `unknown`
with source `authoritative-config-required`; discovery does not perform costly boundary searches.

Each protocol result contains `model_results`. Each model result contains `capabilities`,
`capability_evidence`, `cache_hit`, `probe_requests`, and optional `evidence_error`. Evidence statuses are
`supported`, `unsupported`, or `unknown`; sources may be `probe`, `cache`, `protocol-contract`,
`capability-prerequisite`, or `authoritative-config-required`. A typical full-capability text model needs two
combined inference probes (core text features and tools); deterministic rejections may require focused probes.
Do not infer Custom Provider capabilities from model names, and do not replace explicit configuration with
`unknown` evidence.

Parallel-tools support is proven only when one assistant turn returns two distinct complete function calls with
valid JSON-object arguments. Accepting a parallel request field but returning one call is not sufficient; the
fallback may still prove ordinary `tools` support. The capability registry is contract-tested against all fields
of `domain.Capabilities` so new routing capabilities require an explicit evidence strategy.

The priority capability represents Fast (OpenAI service_tier=priority).
Chat/Responses discovery returns unknown with source authoritative-config-required for this bit;
other protocols return unsupported with source protocol-contract. No inference probe is used to establish
priority: parameter acceptance and low latency do not prove priority processing. Preserve existing
declarations when evidence is unknown. The Console exposes Fast as a declaration and routing predicate.
Codex account model sync updates this bit for new and existing models from service_tiers: explicit
priority enables it, an explicit list without priority revokes it, absent/null preserves the prior declaration.
Static presets and generic catalog refreshes preserve this account/operator declaration. Unknown is not
verified support: do not add a manual true solely to expose Fast. A separate minimal streaming inference
request can establish the returned tier for that request; it does not change the discovery engine's result.
See [Fast evidence and withdrawal](opencodex-fast.md) for the verification and revision-safe cleanup workflow.

Sampling controls, `tool_choice`, prompt-cache accounting and conversation continuity are protocol/runtime
semantics, not separate scheduler capability bits. Audio, embeddings, video, hosted search, file search and
computer use are outside the current Ferry data-plane and scheduler capability contract and therefore are not
reported as probed support. Adding any such capability requires coordinated domain, evidence-registry, probe,
Console declaration and contract-test support.

Conditional override fields include `id`, `name`, `enabled`, `callers`, `protocols`, `weekdays`, `timezone`, optional `time_window`, `required_capabilities`, and either `mode:"custom_route"` with `route.steps` or `mode:"adjust_default"` with `prefer`/`avoid` route identities.

Simulation input can include `caller_id`, `protocol`, `stream`, RFC3339 `at`, and `required_capabilities` (`text`, `images`, `tools`, `reasoning`, `vision`, `priority`, `min_context`, `min_output`).

Quota override:

```json
{"upstream_model":"model-a","used":80,"limit":100}
```

Create an inference API key:

```json
{"name":"client","description":"workload","scope_models":["smart"],"scope_protocols":["openai-chat"],"rate_limit_rpm":60,"monthly_budget_usd":25}
```

## Provider model catalog sync

Provider create, patch and protocol-config payloads accept `model_sync_interval`: `off`, `30m`, or
`1h`. New providers and existing providers without this field use `1h`; an omitted patch field keeps
the current interval. Change the interval through the normal draft and apply flow. Disabled providers
do not run scheduled sync. `off` stops scheduling but still allows manual sync for enabled providers.

`GET /api/providers/{id}` returns the effective interval and a `model_sync` status object. It includes
`status` (`idle`, `running`, `succeeded`, `failed`, `unsupported`, or `disabled`), `last_started_at`,
`last_completed_at`, `last_success_at`, `next_run_at`, safe `last_error_code`/`last_error`, the last
run's `discovered_models`, `added_models`, `updated_models`, `created_logical_models` counts, and
`revision`.

`POST /api/providers/{id}/models/sync` runs once against the active provider and publishes additions.
This is high-risk and requires `--confirm-high-risk` plus the token's server gate, or an authorized
administrator session with CSRF. The response includes counts and separate `added_model_ids`,
`updated_model_ids`, and `created_logical_model_ids` arrays. A concurrent run returns HTTP 409;
discovery/publish failures return an error and remain visible in sync status.

A successful Codex OAuth sync also returns `codex_catalog` with `client_version`,
`baseline_source` (an immutable official Codex commit), `baseline_sha256` (the source file's digest),
and `models` for configured models on synchronized connections. Each row has `connection_id`,
`model`, optional account-catalog `visibility`, `model_priority`, optional
`model_minimal_client_version`, `account_priority`, and `connection_priority`. Model and account
priority use `supported`, `unsupported`, or `unknown`; connection priority is the resulting
Responses capability boolean, not a guarantee of route eligibility or acceleration. Missing
account rows or absent/null `service_tiers` are unknown and retain the stored declaration; an
explicit list without priority revokes it. Hidden configured models receive this reconciliation,
but only `visibility=list` rows may add new models or appear in discovery. An all-hidden valid
catalog succeeds with zero discoveries. The pinned model baseline never grants account priority.
This diagnostic belongs to this response's revision and timestamps; it is not persisted or
returned by later provider GETs. Existing SQLite configuration and aggregate sync state remain
the durable source of truth.

Synchronization reads the authenticated model catalog without running inference capability probes.
It adds newly discovered channel models and missing same-name logical models, retains existing
models and custom routes, and safely rechecks configuration before publishing. Discovered models
resolve capability declarations by priority: authoritative upstream catalog metadata (OpenRouter
model metadata replaces the stored contract of existing models), then the community
[models.dev](https://models.dev) aggregate cached for 24 hours (fills new models and only refines
existing ones additively, never subtracting a configured capability), then the conservative
protocol default. A channel without a supported model catalog reports `unsupported`; a catalog
failure preserves its current models.
Official OpenRouter providers read the canonical provider-level `/api/v1/models` catalog once per
effective credential/authentication identity and fan it out to Chat Completions, Responses, and
Anthropic sibling connections. Anthropic transport-decorated ids shaped as
`anthropic/<vendor>/<model> [1m]` are normalized back to `<vendor>/<model>`; ordinary
`anthropic/claude-*` ids are unchanged.
`POST /api/providers/discover-models` remains a preview and does not publish additions.

Custom providers may select a separate Volcengine Plan control-plane catalog with
`model_sync_catalog` in create, patch, and protocol-config payloads:

```json
{"model_sync_catalog":{"type":"volcengine-plan","plan":"agent","credential_ref":"store:ARK_CONTROL"}}
```

`plan` is `agent` or `coding`. An omitted patch field preserves the source; `null` clears it and
restores normal connection discovery. The field is not supported on Codex OAuth providers.
Provider reads return only these non-secret settings. The catalog source uses the same interval,
sync-now endpoint, additive publication, and status contract.

The referenced local secret contains a JSON object with required `access_key_id` and
`secret_access_key` strings, plus optional `session_token` for STS. Store it through the existing
high-risk secret lifecycle endpoint, passing input by protected file rather than command-line values.
Use independent credentials with permission to read the selected Plan model list. Never reuse an
inference connection's secret; `store:NAME` and `env:NAME` with the same name are also rejected.
Do not request raw credentials in conversation or read them back for diagnostics.

The catalog uses a fixed official HTTPS endpoint with Volcengine V4 signing. It does not send the
inference API key to that endpoint, follow redirects, refresh STS, or silently fall back to another
credential. Missing credentials report `catalog_credential_unavailable`; malformed credential JSON
reports `catalog_credential_invalid`. Errors preserve current models and exclude upstream response
bodies and credential material. Rotating the referenced secret invalidates older in-flight results
and makes enabled scheduled providers due again.

## Runtime settings

`GET /api/settings` returns the runtime-local settings: `debug_request_response` (ledger request/response
debug capture) and `scheduled_model_sync_enabled` (the global provider model sync schedule switch,
default `true`). `PATCH /api/settings` is high-risk and accepts either field:

```json
{"scheduled_model_sync_enabled": false}
```

Pausing the schedule stops due-provider dispatch only; per-provider intervals keep their values, and
manual `POST /api/providers/{id}/models/sync` keeps working while paused.

## Aggregate windows

Overview, usage/billing, model and provider aggregate reads accept:

- `window=24h`: a trailing duration ending at the server's current time; the default is 24 hours.
- `window=24h&to=2026-09-05T00%3A00%3A00Z`: a duration ending at a fixed RFC3339 timestamp. Omitting `window` uses 24 hours.
- `from=2026-09-04T00%3A00%3A00Z&to=2026-09-05T00%3A00%3A00Z`: explicit RFC3339 boundaries, without `window`.

The span must be between 1 ms and 90 days, and `to` cannot be in the future. Invalid timestamps,
`from` without `to`, or a nonempty `window` combined with both boundaries return HTTP 400.
For repeatable reconciliation, choose one historical cutoff and reuse the same bounds. Compare the
echoed `from`/`to` in Overview and usage/billing before comparing their aggregate values. Runtime
health and quota snapshots still describe the current runtime; historical bounds apply to ledger statistics.

## Request filters

`GET /api/requests` supports `window`, `limit`, `offset`, `q`, `conversation_id`, `model`, `provider`, `connection`, `api_key`, `outcome`, `failure_type`, `status`, `revision`, `from`, and `to`. Use URL encoding for every user-controlled value.
Requests appear from ingress with `received`, `waiting`, `active`, `recovering` or `streaming` status before a final outcome. These live rows are excluded from finalized usage and cost analytics.
`GET /api/queue` reports actual gate `waiting` and `running` counts, configured connections, their effective runtime limits, and active client requests. Background jobs share gate capacity. Effective concurrency priority is Connection > Provider > global; global settings use `PATCH /api/settings` with `expected_revision`, and Connection overrides use the normal draft/apply path.
Its existing filter behavior is unchanged: `window` supplies the default bounds, and each explicitly
provided `from` or `to` independently overrides that bound, including during paginated exports.

Request summaries, `client_request` in `GET /api/requests/{id}`, and each detail `attempts` entry
expose `service_tier` (the normalized client request), `response_model` and `response_service_tier`
(observed upstream response metadata). `upstream_model` remains the configured target model.
Empty response fields mean no value was observed; they must not be filled from the request or
treated as confirmation that a processing tier was used. Returned model names do not automatically
change the configured billing rate.

Their `usage.cache_subset_of_input` flag describes cache accounting: `true` means input tokens
already include cache reads/writes; `false` means cache counts are independent of input tokens.
Use this flag when deriving uncached input or total eligible input. New records persist it explicitly;
older records retain the ledger's protocol-based fallback. The flag also applies to the detail
`resource_impact.logical_usage` and `resource_impact.upstream_usage` objects.

## Errors

Structured access errors use `{ "error": { "code", "message", ... } }`. Important codes/statuses:

- `401 INVALID_MANAGEMENT_TOKEN`: missing, expired, disabled, revoked, or invalid token.
- `403 HIGH_RISK_NOT_ALLOWED`: ask the administrator to enable the token switch; do not retry.
- `403 ADMIN_SESSION_REQUIRED`: endpoint is Ferry Console-only.
- `409`: active revision or resource lifecycle conflict; reread before proposing another change.
- `428`: an expected revision is required.
- `503`: a required runtime subsystem is unavailable; reads may still be partially available.

### Scheduled reference pricing

Settings includes a pricing_sync object with settings, loading, last_attempt,
last_success, next_update, error, source, url, version, total and stale. PATCH
/api/settings accepts pricing_sync: {"enabled":true,"source":"litellm","interval_hours":24}.
This settings write requires the existing high-risk gate. Interval is 1–168 hours;
source is litellm (GitHub, default) or models.dev. Automatic updates default to
24 hours, persist to control.db, and retry failed fetches after five minutes.
Manual refresh works while the schedule is paused. A failed update retains the
last successful prices. Current reference estimates in request list/detail are
separate from recorded bills and do not overwrite configured rate cards.

### Backup policy

`GET /api/settings` includes `backup`. `PATCH /api/settings` accepts a complete `backup` object:

```json
{"backup":{"enabled":true,"interval_hours":24,"include_history":false,"s3_prefix":"ferry/scheduled","local_copies":2,"remote_copies":30}}
```

Intervals are whole days (24–720 hours), local copies 1–7, S3 copies 1–90. New deployments use
the `ferry/` prefix; migrated databases may retain their existing `smart-api/` prefix.
An independent instance must use its own destination and retention scope before enabling its timer.
The host-provisioned connection uses the `app-backups` bucket. Configuration-only backups
exclude request and conversation history; full release recovery points are independent. Settings
writes require the existing high-risk permission and `--confirm-high-risk`.

### Scheduled backup S3 connection

`backup.s3` accepts `endpoint` (HTTP(S) origin), `region`, `bucket`, and `path_style`.
Supply write-only `backup.s3_credentials: {"access_key":"…","secret_key":"…"}` when
first configuring an endpoint or rotating credentials. GET returns only an opaque
`credentials_ref`, never either credential. Omit credentials to preserve them at the same
endpoint. Omit `s3` to preserve the connection; send `s3: {}` to use host-provisioned MinIO.
A different endpoint requires both credentials. Existing bucket permissions must allow
PutObject, GetObject and DeleteObject inside the configured `ferry/` prefix.

## Reconciled analytics

`GET /api/analytics?window=24h&to=<RFC3339>&model=<id>&provider=<id>` accepts
an optional model, actual attempted provider, or their intersection. It also supports
explicit `from` + `to` instead of `window`, with the shared 1ms–90 day limit.
One read-only ledger snapshot returns `summary`, `models`, `providers`, `coverage`,
`generated_at`, and natural UTC `timeline` buckets. `bucket_seconds` is 300, 3600,
21600 or 86400 depending on the range; edge buckets are clipped to exact bounds.
Model, provider and timeline costs reconcile to the summary.

The cohort contains finalized logical requests selected by start time in [from,to),
including all their attempts across time boundaries. Provider filters match actual
attempts, not the final serving provider. Global/model quality measures client outcomes;
provider quality measures individual upstream attempts. A filtered provider's client
success metric describes requests that tried that provider and can include recovery
through another provider. Quantiles use successful, positive timing observations and
nearest-rank P50/P95; absent samples are null.

`cost = cash_cost + subscription_cost`. Cash includes recorded PAYG estimates only;
subscription cost includes recorded allocations, not the full subscription invoice.
Subscription reference valuation is excluded. Free attempts and PAYG attempts with a
recorded rate-card version can be priced at zero; missing pricing is exposed separately.
Retry, unsuccessful-attempt and failed-client-request cost subsets can overlap.

Missing attempt history is visible as `__unattributed__`. Its ambiguous recorded cash
estimate is `unclassified_reference_cost` and excluded from accounted spend; only its
recorded subscription allocation is included. No current price is applied retroactively.
Token categories normalize provider protocol semantics but do not invent monetary
components absent from historical records. Synthetic resource rows have no detail link.

Coverage is global even for model/provider intersections. It compares only full UTC
hours in the window and separates rollup-only history from mismatched detail hours.
Earliest detail/rollup timestamps are observations, not guaranteed retention cutoffs.

The compatibility `/api/usage-billing` endpoint returns a detail-only summary,
groups and trend, plus a separate `historical_summary` and global `coverage` in one
snapshot. Its monetary fields preserve legacy final-request accounting; they are
not the analytics attempt-cost contract. Legacy `trend.failed` includes cancellation;
`outcomes` separates failed, canceled and historical success.

Report drilling through `GET /api/requests` supports exact normalized `logical_model`,
`participating_provider`, `attempt_status`, `attempt_failure_type` and `report_error`.
The provider and attempt conditions match the same attempt; legacy `provider` still
matches the final serving provider. All use exact `[from,to)` request start bounds.

## Codex usage reset credits

Subscription usage now includes `account_id` and `rate_limit_reset_credits` with
`available_count`, `total_earned_count` (when reported) and `credits`. Each returned
credit preserves its ID, reset type, status, grant/expiration/redemption times,
title and description. A null expiry is **not reported**, not unlimited validity.
An upstream detail error retains the count and sets `credits:null` with
`details_error`. The returned list can be partial; compare available rows against
`available_count`. The console displays all returned rows without disclosure clicks.

### Automatic expiry policy

`GET /api/settings` returns `codex_usage_reset_auto`, defaulting to
`{"enabled":true,"threshold_minutes":20}`. Ferry Console → Settings → General exposes
the same switch and minute threshold. To configure through this Skill, first read
`/api/access` and `/api/settings`, then send both fields (the object replaces the
whole policy) using the existing high-risk settings operation:

```bash
python3 "$FERRY_CLIENT" management PATCH /api/settings \
  --json '{"codex_usage_reset_auto":{"enabled":true,"threshold_minutes":20}}' \
  --confirm-high-risk
```

Read `/api/settings` again to verify. Minutes must be an integer from 1 to 1440;
set `enabled:false` to pause. Existing user authorization for this concrete policy
change is sufficient; the server still requires the token's high-risk permission.

The server scans enabled Codex OAuth providers/connections at startup and every
30 seconds, independently of an open browser. It chooses the earliest expiring
available `codex_rate_limits` credit with **0 < remaining lifetime <= threshold**,
at most one per account per scan. Missing/invalid expiry and expired cards are
skipped. Fresh upstream data and the current policy/connection are checked before
submission. This automatic path needs no per-card browser confirmation.

The durable journal identifies automatic requests as `system:expiring-codex-credit`.
Manual pending requests and uncertain outcomes block automatic consumption.
Submitted automatic attempts are never automatically replayed, including
`nothing_to_reset` and `no_credit`; unknown results require review of the same
request. Unsubmitted automatic intents may resume after a transient preflight
failure. No local cooldown or quota is reset.

### Manual reset

When explicitly requested to initiate a reset, prepare it with the exact selected
`credit_id` and the `account_id` from a fresh usage snapshot:

```json
{"credit_id":"the-selected-credit-id","account_id":"the-reviewed-upstream-account-id"}
```

The prepare response contains `request`, `confirmation_required:true` and
`console_path`. Return the console link to the user. While an authenticated console
is open, its local journal polling surfaces the confirmation popup. Preparation,
journal polling, dismissing and cancellation never consume a credit. For a manual reset, only the user
confirms the displayed account and credit in that popup; a Management Token cannot
call `/confirm`, even with `allow_high_risk:true`. Do not use the host Codex app's
usage-reset tool: that tool targets a different account and is unrelated to Ferry.

Pending confirmation expires after ten minutes. Ferry has no local usage-percent
threshold: an available `codex_rate_limits` credit can be selected at any usage level,
including 50%. Fresh usage is shown for review; the upstream determines the actual
redemption outcome, including `nothing_to_reset`. Consumption still requires the
same OAuth principal/account and the same provider and connection binding.
Local cooldowns, quota overrides and conversation state are untouched. All clients
sharing the upstream account share its Codex usage windows.

The server persists the attempt before the sole consume POST. Duplicate successful
confirmation returns its recorded outcome. A lost response or restart reserves the
original request and selected credit as `uncertain`. Only an explicit confirmation
of that same request can retry it with the same upstream idempotency key; preparing
another credit on that upstream account is blocked. Never create a new request to
work around an uncertain result. Account/connection changes block submission.

Validation uses fake credentials, temporary stores and closed HTTP transports.
Do not exercise real credits or perform post-deployment online reset tests. Existing
release health/version/auth checks make no configured upstream requests.

### Codex request version

GET /api/settings exposes revision, codex_client_version (configured), codex_effective_client_version, and codex_default_client_version. Publish through POST /api/drafts with base_revision and intent.codex_client_version, validate, then apply with expected_revision. This is not a direct settings PATCH. Empty string follows the last automatic version, or the bundled default when none has been resolved. Versions are restored with configuration revisions and consistently applied to catalog queries and inference headers. Effective version changes invalidate scheduled Codex catalog fingerprints while retaining existing models and custom routes.


The same versioned intent accepts codex_auto_update (boolean, default false) and
codex_release_channel (stable by default, or prerelease). GET /api/settings
returns both plus codex_auto_version and codex_update_status. The automatic
version is managed by the server, not a writable draft intent field. A nonempty
codex_client_version always pins the manual version and prevents background
GitHub requests, even when codex_auto_update is true. To opt in, publish:

    {"base_revision":441,"intent":{"codex_client_version":"","codex_auto_update":true,"codex_release_channel":"prerelease"}}

Use the actual current revision, validate, and apply with expected_revision under
the existing high-risk publish permission. Review all three fields together.
Clearing a manual pin when enabling automation retains its effective version as
the starting automatic version. Leaving auto mode off retains the last automatic
version. All four version fields are restored with configuration revisions.

The startup/minute poll checks eligible configurations hourly. Stable uses GitHub
releases/latest; prerelease selects the highest compatible rust-v version among
the latest 100 releases, excluding drafts. Failed HTTP, timeout, invalid release,
and publication attempts retain the working config and retry hourly. Concurrent
configuration revisions discard fetched results and retry from a fresh snapshot.
Updates never downgrade, and unchanged versions create no revision. Returning
from prerelease to stable waits for a higher stable version.

codex_update_status contains status (disabled/manual/pending/succeeded/failed/
unavailable), last_checked_at, next_run_at, latest_version and last_error when
available. Historical check fields can remain visible while paused; next_run_at
is omitted when manual or disabled. Version changes invalidate the existing
catalog schedule but do not bypass global/provider model-sync switches, model
creation limits, or custom routes. This does not sync desktop OpenCodeX settings.


## Native provider accounts

`GET /api/oauth/native/drivers` lists implemented native drivers and each driver's actual wire protocol,
authentication mode and endpoint. This is an implementation inventory, not proof of account/model capabilities.
For `/api/oauth/native/{driver}/accounts/{account}`:

- `GET /status` reads a redacted local view without polling the provider.
- `POST /login` starts authorization with optional `fields`; browser authorization is completed by the user.
- `POST /complete` submits `{"callback":"..."}` for browser login or `{}` for a device check.
- `DELETE /login` disconnects the account, removing its token and pending login.

All writes require explicit target authorization and the management high-risk gate. Callback URLs,
service-account JSON and credential-bearing fields must use trusted input/file/stdin and never be echoed.
Use returned `credential_ref` and the driver's bound endpoint/protocol in a normal Provider draft;
login does not itself publish a configuration revision or declare model capabilities. Native accounts
cannot redirect their credentials to arbitrary endpoints. Expired nonrenewable sessions require login.
