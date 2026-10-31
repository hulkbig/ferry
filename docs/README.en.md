# Ferry documentation

[中文](README.md) · [Project overview](../README.en.md)

## Start and operate

| Task | Guide |
| --- | --- |
| Install, initialize and send a first request | [Docker installation](guide/docker.en.md) |
| Change models without reconfiguring clients | [Logical models](guide/logical-models.en.md) |
| Connect Hermes to existing accounts | [Hermes Agent](guide/hermes.en.md) |
| Redeem expiring Codex reset credits | [AutoReset](guide/autoreset.en.md) |
| Understand behavior and limitations | [Frequently asked questions](guide/faq.en.md) |
| Diagnose a failed or stalled request | [Troubleshooting](guide/troubleshooting.en.md) |
| Back up, restore and upgrade | [Docker maintenance](guide/docker.en.md#back-up-and-restore) |
| Import accounts and client keys | [sub2api / CLIProxyAPI import](guide/credential-import.en.md) |
| Report a bug or propose a change | [Contributing](../CONTRIBUTING.en.md) |
| Change code and run checks | [Development](development.en.md) |
| Report a vulnerability privately | [Security policy](../SECURITY.en.md) |

These guides have separate Chinese and English editions. GitHub issue forms and the PR template accept either language; you do not need to submit both.

## Detailed references

Some detailed references are currently in Chinese. Their language is identified below rather than presenting them as fully translated.

| Reference | Scope | Language |
| --- | --- | --- |
| [Long tasks](guide/long-tasks.md) | Recovery routes, budgets and verification | Chinese |
| [Configuration](guide/configuration.md) | Models, connections, routes and identities | Chinese |
| [Client APIs](guide/api.md) | Chat, Responses, Messages, streaming and images | Chinese |
| [Agent Skill](guide/agent-skill.md) | Setup, dedicated identities and controlled changes | Chinese |
| [Provider coverage](provider-channel-coverage.md) | Authentication and verification boundaries | Chinese |
| [Protocol fidelity](protocol-fidelity.md) | Preserved, translated and rejected fields | Chinese |
| [Provider adapters](provider-adapters.md) | Authentication, translation and provider boundaries | Chinese |
| [Native drivers](reference/native/README.md) | Login, refresh and driver limitations | Chinese |
| [Migration](guide/migration.md) | Existing SmartAPI data and client migration | Chinese |
| [Third-party notices](third-party-notices.md) | Dependencies and licenses | Chinese |

[Website source and preview instructions](../website/README.md) are in Chinese. The product website itself supports English and Chinese. The [public website](https://ferry.bighulk.dev/) is hosted on GitHub Pages with HTTPS.

[Codex usage protection](guide/codex-usage-protection.en.md): default 98% threshold, paid Credits off, response quota evidence reused before fetching usage.

- [性能与资源 / Performance](guide/performance.en.md)
- [Headless](headless.en.md)
