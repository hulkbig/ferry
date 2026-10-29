# Security policy

[中文](SECURITY.md)

## Reporting a vulnerability

The first public release is being prepared. A public private-reporting channel has not yet been enabled. Enabling and verifying that channel is a release prerequisite; a link alone does not establish that it works.

After GitHub private vulnerability reporting is enabled, use **Security → Report a vulnerability** in `hulkbig/ferry`: [private report entry](https://github.com/hulkbig/ferry/security/advisories/new). If it is unavailable, use an existing private channel to the maintainer or request a private contact without disclosing vulnerability details. Do not submit exploit steps to a public issue or PR.

Do not attach databases, backups, tokens, complete OAuth callback URLs or user conversations. Ordinary functional bugs belong in [Issues](https://github.com/hulkbig/ferry/issues); see [Contributing](CONTRIBUTING.en.md).

## What to include

Provide the affected version, installation method, impact, a minimal reproduction, necessary redacted logs and whether actual exposure has occurred. Start with a summary, then exchange sensitive reproduction material privately. Do not test instances you are not authorized to assess.

## Supported versions and response

No supported public version has been released yet. With the first release, this policy must name the supported version and the reporting channel must be verified. Older development snapshots do not have a separate support commitment.

There is no guaranteed response or remediation deadline. Maintainers first establish impact and reproduce the issue, then coordinate remediation and disclosure. A candidate build or passing CI is not evidence of a completed security audit.

## Deployment boundaries

- Initialize the administrator in a controlled environment before enabling access.
- Use TLS and access restrictions for remote administration; avoid exposing management ports directly to the internet.
- Use separate, scoped client API keys and management tokens.
- Protect SQLite, WAL files, backups and request logs; encryption at rest is not enabled by default.
- Request-body debugging can contain sensitive data. Enable it only when needed and review retention.

See [Docker operations](docs/guide/docker.en.md). The detailed [threat model](docs/security-threat-model.md) and [operations guide](docs/guide/operations.md) are currently in Chinese.
