# Troubleshooting a stalled or failed request

[中文](troubleshooting.md) · [FAQ](faq.en.md) · [Docker installation](docker.en.md)

Start with one request in **Requests**, identified by its timestamp or ID.

| Observation | Check next |
| --- | --- |
| No upstream attempt | Identity, routing, protocol/capability preflight; queue capacity if waiting |
| A failed attempt followed by success | Failed route, recovery trigger and successful route; an intermediate failure is not necessarily a failed client task |
| Failure after waiting or recovery | Compatible candidates, allowed recovery conditions, budget and client cancellation |
| Partial output followed by interruption | Final upstream attempt and stream terminal event; Ferry does not splice a replacement stream after output is committed |

The [long-task guide](long-tasks.md) (Chinese) covers recovery routing. Confirm which instance you are inspecting: native defaults are management `7900` and inference `7890`; release Compose exposes `17910` and `17810` on the host. Custom installations may differ.

## YAML edits have no effect

YAML is imported only into an empty database. Initialized instances use SQLite configuration, edited through the console or management API. Do not delete the database to apply routine configuration changes.

## Address already in use

Check for another process on the same address. Restored configuration includes its original listener addresses. Inspect it with `ferry -data-dir <directory> -startup-check`, or rehearse restoration in an isolated environment instead of conflicting with the original instance.

## The client receives 401 or 403

Use an inference API key for `/v1/*` and a management token for `/api/*`. Check whether the identity is enabled, expired or revoked, and its model/protocol scope. Management writes may also require session CSRF validation or the token's high-risk gate.

## A model is listed but cannot be called

Discovery is not inference verification. Check logical-model routes, Provider/Connection status, credentials, account access and protocol. Inspect routing failures and upstream attempts in Requests. A model name alone is not evidence for tools, reasoning or Fast support.

## Normal requests work, but streaming or tools fail

Check endpoint capabilities and request parameters. Look for event-stream buffering by a proxy, missing terminal-event handling in the client, or lost tool-call IDs in follow-up messages. Cross-protocol parameters that cannot be represented may be rejected. Reproduce against the same instance, model and exact protocol.

## No upstream output, but no failover

Distinguish queue waiting from an active upstream call. The model's `first_output_timeout_seconds` defaults to `0`, disabling the first-output timeout. Enabling it also requires a routing policy that permits timeout recovery and an eligible candidate. Timing starts after concurrency capacity is acquired and ends on the first effective text, reasoning or tool output. It does not cut off a healthy stream just because the full response is unfinished.

See the [management API](../../skills/ferry/references/management-api.md) and [long-task guide](long-tasks.md) (Chinese) for configuration.

## A native account is signed in, but the provider is unavailable

Signing in saves account credentials; saving and enabling the Provider are separate steps. Reopen the editor to read current account state. `relogin_required` needs another login; Meta's `key_pending` requires explicitly obtaining an inference key. Vertex, Antigravity and Devin use exact model IDs instead of relying on default catalog synchronization. See the [driver references](../reference/native/README.md).

## A container cannot reach an upstream

Inside a container, `localhost` refers to that container. Use an address reachable from inside it and check DNS, TLS, proxies and authentication. Do not expose management ports as a connectivity workaround.

## Dark mode does not follow the system

The theme button cycles through System, Light and Dark. An explicit theme overrides the operating system preference; switch back to System. Theme and language are stored in the current browser and do not synchronize between browsers.

## The console still looks old after an upgrade

Console assets are embedded in the binary. Editing source files does not update the running process. Rebuild and restart the target instance, refresh the browser and check `/version` to confirm the served version.

## Costs differ from the upstream invoice

Check the time range, model prices, cached tokens, failed/fallback attempts and subscription allocation. Missing prices or usage evidence do not mean zero cost. Console estimates do not replace provider invoices, and a service-tier response alone does not prove acceleration or extra billing.

## What should a report include?

Give the version, installation method, redacted configuration, reproduction, expected result, actual error and request time/ID. Reduce the input to a minimal example. Do not attach databases, backups, tokens or real user conversations. Follow [SECURITY.en.md](../../SECURITY.en.md) for suspected vulnerabilities.
