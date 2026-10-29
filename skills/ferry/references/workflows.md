# Ferry operating workflows

## Inspect and triage

1. Read `/api/access`, `/api/overview`, and `/api/alerts`.
2. If a model needs attention, read `/api/models/{id}` and its providers before changing anything.
3. If a request failed or recovered, read `/api/requests/{id}`. Distinguish client outcome, upstream attempt failure, connection recovery, and provider fallback.
4. Use model or draft simulation to prove eligibility and exclusions without sending upstream traffic.
5. For OAuth capacity, use `oauth/usage?refresh=false` first. Refreshing performs a read-only upstream query.

## Revision-safe model or provider change

Most model/provider/connection mutation endpoints return `202` with a `draft_id`; they do not immediately alter the active runtime.

1. Make the narrow draft mutation.
2. `GET /api/drafts/{draft_id}` and note `base_revision`.
3. `POST /api/drafts/{draft_id}/validate`.
4. `POST /api/drafts/{draft_id}/simulate?model={logical_model}` when routing can change.
5. Explain the exact before/after effect. Reuse explicit authorization for this target and change in the current task; seek approval only when the effective change exceeds it.
6. Apply with `POST /api/drafts/{draft_id}/apply`, body `{ "expected_revision": <base_revision> }`, plus `--confirm-high-risk`.
7. Read the changed resource and `/api/revisions/{new_revision}` to verify it.

Do not automatically replay an apply after `409`. Reread the active revision and rebuild the proposal; seek renewed review only when the effective change exceeds existing authorization.

## Routing-specific review

Use `POST /api/models/{id}/routing/review` with ordered route steps and the existing-conversation strategy. Keep `recover_on` empty on the final step. Review:

- actual provider and upstream model at every step;
- typed failure conditions that advance to the next step;
- candidate compatibility and available connection count;
- current traffic and active-conversation impact;
- the returned `draft_id` and `expected_revision`.

Apply the returned identifiers at `/routing/apply` only after explicit approval.

## Provider credentials

Store new upstream material through `/api/secrets/{name}` and refer to it as `store:{name}`. Secret reads and writes are high-risk. Never put the raw value in a summary.

Connection testing and protocol discovery can issue real upstream requests and may consume quota or money. Tell the user before probing a large model set.

Credential rotation changes new attempts immediately. OAuth import accepts the `auth_json` document but does not retain the uploaded file; OAuth API responses remain redacted.

## API-key lifecycle

API keys are inference identities, not Management Tokens.

- Create/rotate responses may contain a one-time secret. Show it only through the original command output, instruct secure storage, and do not repeat it.
- Disable is reversible. Revoke is permanent and invalidates authenticating secrets.
- A rotated old secret enters grace until explicitly retired. Confirm clients migrated before retiring it.
- Inspect identity usage and recent requests before narrowing scope, lowering limits, disabling, revoking, or deleting.

All API-key lifecycle operations are high-risk even when the endpoint uses `GET` to explicitly reveal a secret.

## Manual runtime overrides

`clear-cooldown` supports global (`POST /api/runtime/clear-cooldown` with an empty body), Provider, connection, upstream model, and logical model scopes. The runtime endpoint accepts optional `provider_id`, `connection_id`, and `upstream_model` JSON filters; filters intersect, and a scope matching nothing returns 404. Unknown fields or conflicting path/body filters return 400.

Reset clears selected model cooldown/breaker/local quota observations and the owning connections' shared quota, auth/reachability failure, and admission throttle gates. Sibling model cooldowns remain intact for a model-only reset. It preserves configured enablement/concurrency, in-flight requests, and durable usage accounting. Success returns `cleared_models`, `cleared_connections`, and `persisted: true` after the cooldown file is saved; a write failure returns 500 and states that the memory reset was applied. A subsequent upstream rejection or explicit fresh quota query can park the resource again. Cached OAuth usage reads are display-only and cannot undo a reset.

Connection availability belongs to an internal incarnation plus the credential/account binding, not its display name. Rebinding a credential, rotating the key at the same reference, or deleting/recreating a connection invalidates old state. A normal OAuth token refresh for the same account preserves ownership. Cooldown persistence format 2 records ownership fingerprints; the first upgrade intentionally discards unversioned cooldown files because their account ownership cannot be verified. Future restarts restore only matching owners. Do not edit a running process's cooldown file: use the reset API.

`set-quota` applies to one `upstream_model` and records a high-confidence manual observation. These actions change scheduling immediately without creating a configuration revision, so use them only for a concrete incident and verify the connection view afterward.

## Management Token administration

The Skill must not create, rotate, elevate, revoke, or delete Management Tokens. An administrator performs those operations in **Ferry Console → Settings → Security** with an authenticated session and CSRF protection. The raw token appears only after create or rotate; the server stores a SHA-256 verifier and safe preview.
