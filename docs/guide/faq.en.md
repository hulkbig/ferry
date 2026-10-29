# Frequently asked questions

[中文](faq.md) · [Docker installation](docker.en.md) · [Troubleshooting](troubleshooting.en.md)

## Is Ferry a client or a model service?

Ferry is a self-hosted AI gateway between clients and upstreams. Keep using Claude Code, Codex, OpenCode, Pi or another client, and configure its Ferry endpoint, key and logical model. Protocol support, tool calls and follow-up turns depend on the actual route.

## Are accounts, models or free credits included?

No. Bring your own API keys or supported subscription accounts. Signing in with a subscription does not grant access to every model. The upstream provider's account terms and charges still apply.

## What is the simplest installation method?

Use the [two-command Docker Compose installation](docker.en.md): download the versioned Compose file, then run `docker compose up -d`. Create the administrator in the console, add an upstream, check routing and create a client key. No source checkout, Node.js, external database or OpenBao is required. Before the first release, the images and download links are not yet available for external installation.

## Why can't I open the console on my server?

The default Compose file binds ports to the host's loopback interface, so other devices cannot access them directly. Initialize through an [SSH tunnel](docker.en.md#remote-server), then configure HTTPS and access restrictions for ongoing remote use. The host management port is `17910`; inference uses `17810`. They are separate endpoints.

## Why does a listed model fail when called?

Model discovery only establishes that a name was found. It does not verify the account, endpoint or protocol. Check key permissions, upstream credits or quota, the exact model ID and Provider/Connection status, then inspect routing and attempts in Requests. [Verification coverage](../provider-channel-coverage.md) (Chinese) varies by provider.

## Will it keep Thinking after a rate limit until it succeeds?

Eligible requests can wait within a recovery window and continue when conditions allow. A client's Thinking indicator does not mean continuous upstream reasoning or unlimited waiting. Recovery is bounded by time, attempts, eligible routes and client cancellation. Waiting may not fix missing permissions or an unpaid account.

## Can it switch upstreams seamlessly halfway through a response?

That is not guaranteed. Once visible output has been committed to the client, Ferry does not splice a replacement upstream stream into it. Recovery and failover respect output boundaries and routing policies; they cannot repair every interruption. See the [long-task guide](long-tasks.md) (Chinese) for configuration.

## Does concurrency control prevent account suspension?

It limits simultaneous requests and queues traffic when capacity is unavailable. This helps manage request pacing, but does not guarantee protection from rate limits or suspension. Follow your upstream provider's account and usage rules.

## Are the three protocols fully interchangeable?

Ferry exposes Chat, Responses and Messages endpoints. Translation is constrained by the target protocol and model. Unsupported fields may be rejected or degraded according to documented rules. Verify streaming, tool calls and follow-up turns with your own client before relying on a route. See [protocol behavior](../protocol-fidelity.md) (Chinese).

## Can recovery cost more? Are cost reports provider invoices?

Recovery attempts, capability probes and scheduled messages may cause extra upstream calls. Ferry estimates costs from recorded usage and configured prices; it does not replace provider invoices. Missing price or usage evidence does not mean a request was free.

## Where is data stored? Does replacing the container delete it?

The release Compose file uses the `ferry-data` volume mounted at `/data`, with databases in `/data/state`. Replacing the container preserves the volume; deleting the volume deletes its data. Databases contain configuration, credentials and request records. Default backups contain only current configuration and recovery credentials; add `-backup-history` explicitly for full history. Neither is encrypted at rest by default. Use protected local storage. Do not use `docker compose down --volumes` for routine shutdowns.

## Can replicas share a database? Is there a multi-tenant console?

Ferry currently suits individuals and small teams using a single instance and one administrator. Run one process per local SQLite data directory. Shared-data replicas and a multi-tenant administrator console are not supported.

## How do I upgrade, back up and restore?

Pin an image version and create and verify a backup before upgrading. Follow the [Docker backup and restore steps](docker.en.md#back-up-and-restore); do not copy live database files. Restoring replaces data without merging later writes; configuration backups leave history empty, while explicit full backups restore history from the backup point. Reverting the image does not undo a database migration.

## Why did editing YAML or changing the system theme have no effect?

YAML is imported only into an empty database; after initialization, use the console or management API for configuration changes. An explicit light or dark theme overrides the system preference; select System to follow it again. See [troubleshooting](troubleshooting.en.md).

## How do I report a bug, submit a PR or report a vulnerability?

Use [Issues](https://github.com/hulkbig/ferry/issues) for bugs and feature requests. Include the version, installation method, a redacted reproduction and request ID; never attach databases, tokens or real conversations. Small fixes, translations and documentation can go straight to a PR; see [Contributing](../../CONTRIBUTING.en.md). Report suspected vulnerabilities privately as described in the [security policy](../../SECURITY.en.md). English and Chinese are both welcome.
