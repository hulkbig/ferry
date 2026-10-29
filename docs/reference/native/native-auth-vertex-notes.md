# Native Google Vertex service-account authentication

Implemented 2026-10-23 in `internal/nativeauth/vertex.go`; tests are in
`vertex_test.go`. Constructor: `NewVertexDriver() Driver`. The default manager registers this driver; the runtime transport enforces its
account binding, and the Provider editor supports explicit service-account import.
The shared Driver contract is unchanged.

## Explicit import

Call Begin with `LoginOptions.Fields`:

| Field | Requirement |
| --- | --- |
| `service_account` | Required JSON string containing the user's actual service-account credential, not a filename |
| `location` | Required by the driver; the UI explicitly supplies `us-central1` by default, editable to another region or `global` |
| `project_id` | Optional explicit target-project override; otherwise the JSON's project_id is used |

The JSON must have type `service_account`, a valid project_id, client_email,
private_key, and token_uri exactly `https://oauth2.googleapis.com/token`.
private_key_id is optional; if present it is used as the JWT key ID.
universe_domain must be absent/empty or `googleapis.com`. Ordinary additional
Google export fields are tolerated but discarded; auth_uri, certificate URLs,
and other imported URLs are never fetched.

Begin validates the import without network access, preserves State and Verifier,
and returns a ten-minute session with no URL or polling interval. The UI offers an explicit Complete import action, which calls
`Complete(ctx, client, session, "")`; no browser callback is required and
the callback argument is ignored. Expired/missing sessions return
ErrLoginRequired. Complete obtains the actual OAuth access token before success.
Neither method reads environment credentials, host files, metadata services,
gcloud state, ADC, external-account federation configuration, or a private-key
URL. Missing fields fail with an explicit static error instead of dummy values.

## Validation boundaries

Imports are capped at 64 KiB; duplicate top-level JSON members, trailing JSON,
non-object JSON, and invalid typed fields fail. Private-key PEM is capped at
16 KiB and must contain one unencrypted PKCS8 or PKCS1 RSA private key. Accepted
keys have two primes, 2048–4096 bits and exponent 65537; key consistency is
validated. Unsupported keys, including EC and encrypted PEM, fail locally.

Project IDs must be 6–30 lowercase ASCII letters/digits/hyphens, beginning with
a letter and ending with a letter or digit. Numeric project numbers and legacy
domain-scoped IDs are not supported. client_email must use the modern
`ACCOUNT@PROJECT.iam.gserviceaccount.com` format; legacy service-account email
formats are not supported. The email's project can differ from the target
project; Google IAM decides whether the account may access that project.

Location must be `global` or a lowercase regional identifier such as
`us-central1` or `europe-west4`, at most 63 characters. This is a structural
allowlist, not a claim that every matching region exists or supports a model.
Slashes, dots, ports, query strings, zones and URL syntax are rejected. Stored
Session/Token metadata is revalidated before signing or deriving a binding.

## OAuth exchange and refresh

The driver creates an RS256 assertion with the imported email as issuer,
`https://oauth2.googleapis.com/token` as audience, and the fixed
`https://www.googleapis.com/auth/cloud-platform` scope. iat uses the local clock;
exp is one hour later. No delegated subject or impersonation claim is included.
It sends the assertion using the JWT bearer grant, never the raw private key.
Refresh signs and exchanges another assertion; it does not use a refresh-token
grant. This implements Google's documented
[service-account OAuth exchange](https://developers.google.com/identity/protocols/oauth2/service-account).

AccessToken contains only the returned bearer; RefreshToken and Identity remain
empty. ExpiresAt uses the request-start time plus the server's expires_in.
Responses must advertise Bearer type and a lifetime of 1–3600 seconds. The caller
should refresh before expiry and maintain an accurate system clock.

**Session.Extra and Token.Extra are confidential in their entirety**, including
`service_account` (canonical JSON containing the private key), `project` (target
project), and `location`. They must remain in the existing protected secret store
and must never appear in public views, logs, URLs, traces, or exception text.
Only the fields necessary to sign again are retained. Returned token maps do
not alias input maps. Failed exchanges return an empty Token; the caller retains
the prior saved credential until a successful replacement.

## Integrated dynamic binding

Info returns ID `vertex`, Mode `import`, Protocol `openai-chat`, and an **empty
BaseURL**. A shared projectless endpoint would be unsafe. The driver also
implements these optional methods without changing the Driver interface:

```go
interface {
    Binding(Token) (baseURL string, protocol string, err error)
    ValidateEndpoint(baseURL, protocol string) error
}
```

Binding validates confidential token metadata and derives:

- Regional: `https://LOCATION-aiplatform.googleapis.com/v1/projects/PROJECT/locations/LOCATION/endpoints/openapi`
- Global: `https://aiplatform.googleapis.com/v1/projects/PROJECT/locations/global/endpoints/openapi`

The native request is `POST <baseURL>/chat/completions` with
`Authorization: Bearer <AccessToken>` and `Content-Type: application/json`.
Use JSON or SSE Accept headers according to the actual request. Google's
[Chat Completions sample](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/samples/generativeaionvertexai-gemini-chat-completions-non-streaming)
shows the regional OpenAPI path and cloud-platform scope; the official
[Vertex inference examples](https://github.com/GoogleCloudPlatform/vertex-ai-samples/blob/main/skills/vertex-inference/SKILL.md)
include the global OpenAPI path.

ValidateEndpoint performs static configuration validation without secret-store
access: protocol must be openai-chat and the URL must exactly match the canonical
Google host and v1 project/location OpenAPI shape. Regional host and resource
location must match. No ports, userinfo, escapes, query, fragment, trailing slash,
or appended operation path is allowed. It validates syntax, not account access.

The manager registers the driver and configuration validation uses ValidateEndpoint
before secret-store access. At runtime, Binding resolves the saved account's exact
endpoint; the native transport verifies it again before attaching the bearer.
Authorized View responses expose the bound `base_url` and `protocol`, which the
Provider editor uses after successful import. The transport retains the
`/chat/completions` operation and blocks redirects. It does not fall back to a
projectless Google host or let an editable URL override the account binding.
The derived project/location URL is exposed only on authorized configuration
surfaces. The UI clears service-account JSON on submission and stores only the
`native-oauth:vertex:<account>` reference in the Provider draft.

## Capability and HTTP limits

The native upstream contract is OpenAI Chat. Existing Ferry ingress conversion
policy remains available, but this driver adds no Gemini generateContent
transformation or native Responses/Anthropic wire,
model discovery, beta endpoint selection, hosted tools, media conversion,
impersonation, or quota-project headers. API enablement, IAM permissions, billing,
model availability and regional feature support must be checked separately;
a successful token exchange is not an inference capability probe. Model catalog
discovery is unsupported; the editor defaults sync to `off` and requires explicit
model IDs for capability probes.

Only the supplied HTTP client is used for exchanges. A shallow copy preserves
its transport/timeout, disables its cookie jar, and blocks all redirects without
mutating the caller. The transport itself is a trusted caller dependency. There
is a 30-second request-context timeout, a 64 KiB response cap, and a 16 KiB bearer
limit. Bodies are closed; transport, JSON, provider, and body-read errors are
sanitized. Arbitrary response bodies and error descriptions are never returned.
No retry or background task is started by the driver.

## Evidence and attribution

Verified reference: [CLIProxyAPI `d946c4fba6fc9a5cfbca1f3d85184d62ad2fb683`](https://github.com/router-for-me/CLIProxyAPI/tree/d946c4fba6fc9a5cfbca1f3d85184d62ad2fb683), This is the inspected revision, not a claim about the latest upstream version.

The Vertex implementation was newly written against the existing nativeauth interface
and the official protocol documentation linked above; no external source bodies
were copied. Tests generate ephemeral RSA keys and use closed fake RoundTrippers
only, including signature verification, claims, refresh, native bindings,
endpoint/import rejection, redirects, cookies, secret-safe errors, response
limits, cancellation, expiry and absence of environment fallback. No Google
credential or live token/inference request was used.

Validation commands:

```sh
go test ./internal/nativeauth -run '^TestVertex' -count=1
go test -race ./internal/nativeauth -run '^TestVertex' -count=1
go build ./internal/nativeauth
```
