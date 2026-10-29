# Ferry Console v3 — Production Console and Demo

A dependency-free B-side control plane implementing the product model agreed in the 2026-08-26 review: **Route · Recover · Explain**. The same reviewed assets are embedded by the Go management server with the real HTTP adapter, while standalone use remains an in-memory demo.

## What is included

- Overview — runtime/API availability is the headline while client outcomes remain independent; Direct / Recovered / Failed, usage, cache efficiency, traffic trends and recent events are shown without duplicate health-summary cards.
- Models & Routing — model list with first-token/completion timing, Model Workspace, route rail, live traffic, and complete ordered Override CRUD.
- Routing change flow — `Edit → Review → Apply → Revision`, with explicit per-step recovery failures and selectable existing/new conversation behavior.
- Route Simulation — caller, protocol, streaming, capability and RFC3339 time inputs; matched Override, effective route, candidate exclusions and expected selection output.
- Providers — shared endpoint/key, independently timed four-protocol discovery (Chat, Responses, Anthropic, Images), bounded capability verification with exact-key evidence reuse, per-protocol overrides, native model groups and request-protocol-first Ferry conversion fallback.
- Provider Workspace — serving status with the last successful request time and a complete saved edit flow.
- Connection Inspector — status, capacity, last issue, moved requests and active conversation bindings.
- Add Provider / Add Connection wizards.
- Requests — one row per client request, not one row per internal attempt.
- Request Execution Trace — routing decision, connection attempts, recovery/fallback and final response.
- API Keys — masked list/detail, scope and limits, explicit reveal/copy, create/rotate, Grace retirement, disable, revoke and deletion.
- Settings — single-runtime boundary, usage/billing, audit/revisions, administrator security, and complete Management Token lifecycle.
- Runtime Insights — deterministic context-aware explanations and links without an external LLM.
- Demo scenarios — Healthy, Protected, Provider Exhausted and Model Unavailable.
- Mock/API adapter boundary — the embedded management console uses the same-origin HTTP adapter; standalone use defaults to mock data.

## Run

No build step and no third-party runtime dependencies are required.

```bash
npm test
npm run serve
```

Then open:

```text
http://localhost:4173/#/overview
```

The standalone default is the in-memory mock adapter. To point it at the versioned management API:

```text
http://localhost:4173/?adapter=http&base=http://localhost:8080#/overview
```

The Go management server serves these exact assets at `/` with `data-default-adapter="http"`. Append `?demo=1` to explicitly enter demo mode.

## Main routes

```text
#/overview
#/models
#/models/gpt-5.6-sol
#/providers
#/providers/codex-personal
#/requests
#/requests/req_01H8K721
#/api-keys
#/settings
```

## Product invariants encoded in the prototype

1. A client request is the primary user-visible object. Internal attempts remain inside its Execution Trace.
2. Connection recovery is distinct from provider fallback.
3. Provider health, model protection and client experience are separate layers.
4. Recovered success is a first-class success category, not an error.
5. Runtime-changing configuration uses `Edit → Review → Apply → Revision`.
6. Existing conversations can remain on their current route while new conversations use the new revision.
7. API keys are masked by default; administrators can explicitly reveal or copy an active key after creation. Legacy hash-only keys require one rotation before reveal is available.
8. Automatic recovery events that cause no client impact are shown as managed automatically, not as critical alerts.
9. Ordered Overrides are first-match; unmatched requests use the Logical Model default route, and broader rules cannot silently shadow later enabled rules.
10. Operational route progression follows the explicit failure types on each non-final step; pre-commit compatibility failures advance automatically, while client errors and the final step fail closed.
11. Only the unversioned `/api/*` surface is a product management API. `/api/v2/*` and retired legacy-only paths return `404`.
12. Ferry Console uses the administrator Session and CSRF; an independent Skill uses a Management Token whose high-risk operations are enabled per token in the console.

## Project structure

```text
index.html            Application entry
app-loader.js         Loads and assembles review-friendly app fragments
app.part*.jsfrag      Router, pages, interactions, scenarios and Copilot UI
styles.part*.css      Design tokens and complete component/layout styling
mock-data.js          Stable demo ViewModels and scenario fixtures
api.js                FerryAdapter + MockFerryAdapter + HttpFerryAdapter
smoke.mjs             Dependency-free adapter behavior smoke tests
http-adapter.test.mjs  Real API transformation, errors, cooling, trace and write-flow contract tests
ui-state.test.mjs      Loading, empty, partial/full error and 409 recovery source contract
verify-source.mjs     Reassembles fragments and runs Node syntax validation
API-CONTRACT.md       Production ViewModel and management API contract
CODEX-HANDOFF.md      Concrete integration checklist for Codex
```

The source is split only to keep the GitHub handoff reviewable; `app-loader.js` assembles it into one browser module at runtime. Codex is free to consolidate the fragments when integrating into the production front-end toolchain, as long as the page-facing adapter and product behavior remain unchanged.

## Runtime and scope boundary

`HttpFerryAdapter` consumes the implemented `/api/*` management facade. Runtime-changing writes create and validate durable SQLite drafts, then apply with optimistic concurrency. Request, usage and recovery evidence comes from the Ledger; OAuth reuses the existing Codex flow; secret values use the existing SQLite secret store. `/api/auth/*` is browser-session-only and Management Token administration is never available to a bearer token.

The current backend is a single local runtime. Real mode explicitly reports that organization and member isolation are unavailable and never manufactures multi-tenant behavior. The standalone demo changes no deployment configuration or production state.
