# Import accounts and keys from sub2api / CLIProxyAPI

[中文](credential-import.md)

In **Settings → Import & Migration**, an administrator can upload supported export files, preview their contents and import selected accounts or client keys. Imported providers and client keys remain **disabled** until reviewed. Ferry does not change the source service, take over traffic, read arbitrary server paths or contact the source management API.

## Supported formats

| Source | Supported content |
| --- | --- |
| sub2api `sub2api-data` / `sub2api-bundle` v1 | Account exports, including the `data` wrapper; credentials come from `accounts[].credentials` |
| sub2api OAuth | OpenAI accounts mapped to Codex and Anthropic accounts mapped to Claude; access and refresh tokens are required |
| sub2api API keys | OpenAI, Anthropic and xAI keys and endpoints |
| CLIProxyAPI configuration | Legacy `openai-compatibility` / `api-key-entries`, `claude-api-key`, `codex-api-key`, and v8 `api-keys.<provider>[].keys[]`; Codex keys need an explicit endpoint |
| CLIProxyAPI OAuth files | One or more raw Codex / Claude auth JSON files; listing metadata is not a credential export |
| Client keys | CLIProxyAPI legacy `api-keys: []` or v8 `access.api-keys`; a separate JSON array for sub2api |

Gemini / Vertex native keys, other OAuth providers, Codex SIWC and different OAuth clients are unsupported. Unsupported entries are reported without saving their credentials. Proxies, routes, model aliases, exclusions, extra headers, retry rules, group permissions, budgets, balances and expiry policies are not migrated. Review them separately before enabling access.

Endpoint conversion preserves source request-path semantics. Preview shows the resulting endpoint and authentication header type; importing successfully does not establish endpoint compatibility or account eligibility. Detailed format references and conversion rules are in the [Chinese reference](credential-import.md).

## Import and review

1. Sign in and open **Settings → Import & Migration**. The provider page's import action opens the same tab.
2. Choose the source and a stable source identifier. Upload the export/configuration, optional OAuth JSON files and optional client-key file.
3. Parse and preview. This step writes no credentials and makes no upstream requests. Inspect redacted entries, unsupported items and warnings.
4. Select entries and import them in the disabled state. Inspect each result.
5. Review endpoints, proxies, model discovery, routing and client permissions, quotas and expiry. Verify the actual account and route before enabling it. Importing does not create business routes or capability evidence.

A supplemental client-key file is a JSON array:

```json
[{"name":"my-client","key":"REPLACE_WITH_SOURCE_CLIENT_KEY"}]
```

Each request allows at most 500 items and 4 MiB of file content in total. Files remain in page memory, not browser persistent storage. Completing the operation clears the selection; leaving the tab, signing out or closing the page releases requests and file references. Old `/import.html` bookmarks redirect to `/#/settings/import`.

## Retries, partial failures and refresh tokens

Keep the source identifier and entry order unchanged when retrying the same export: those values determine stable object IDs. Existing IDs report `exists` without replacing credentials or configuration. Renaming the source or reordering entries can create new objects; it is not an update mechanism. Client-key imports also check hashes against existing ACTIVE/GRACE credentials.

Entries are saved independently. A partially failed batch returns HTTP 207; successful entries are not rolled back. Credentials saved before a configuration failure remain available for reconciliation, and the response includes their `credential_ref`. Retrying uses a new credential name without overwriting the previous record. An administrator must review unbound credentials before cleanup.

When a request's result is unknown, preview again and check existing objects before retrying. The UI does not automatically replay writes. Re-importing does not rotate or update OAuth credentials.

Imported Codex / Claude credentials have a persistent waiting marker that prevents background refresh, including across restarts. A successful on-demand refresh during explicit probing/use starts their normal refresh cycle. Coordinate with the source instance before enabling use of the same rotating refresh token; Ferry does not stop that instance. Unknown expiry is treated as expired and requires refresh on first use.

## Management API and verification

`POST /api/imports/preview` parses without writing. `POST /api/imports/apply` reparses and imports only IDs explicitly listed in `selected`. Both require management authentication and CSRF protection where applicable; Management Tokens need administrator-granted `allow_high_risk`. Responses use `Cache-Control: no-store` and do not return tokens or keys. The request and response schema is documented in the [detailed reference](credential-import.md#管理-api).

Offline tests cover parsing, redaction, unsupported inputs, permissions, duplicate and concurrent imports, disabled persistence, partial failures, refresh waiting across restarts and UI behavior. Real source exports, OAuth refresh and upstream inference still require verification with the accounts you intend to use.
