# Contributing to Ferry

[中文](CONTRIBUTING.md)

A reproducible tool-call failure, accurate provider capability evidence or a clearer installation step can help the next person using Ferry. Contributions and discussions in English or Chinese are welcome.

## Choose an entry point

- Installation and usage: start with the [FAQ](docs/guide/faq.en.md) and [troubleshooting guide](docs/guide/troubleshooting.en.md).
- Reproducible defects: use the [bug report form](https://github.com/hulkbig/ferry/issues/new?template=bug_report.yml).
- Features or substantial design changes: open a [feature request](https://github.com/hulkbig/ferry/issues/new?template=feature_request.yml) with the use case and existing alternatives before investing in an implementation.
- Small fixes, translations and documentation improvements: a PR is welcome without a separate issue.
- Security vulnerabilities: follow the [security policy](SECURITY.en.md); do not post a public reproduction.

Public collaboration takes place in `hulkbig/ferry`. These GitHub entry points become available with the first public release.

## From a change to a PR

1. Fork the public repository, clone your fork and create a topic branch from the repository's default branch. The first release uses `main`; check GitHub for subsequent changes.
2. Follow the [development guide](docs/development.en.md). Keep the change focused on one problem and leave unrelated formatting, generated files and dependency updates out of it.
3. Add a regression test for changed behavior and update the relevant documentation. Translations should preserve limitations and examples, not just headings.
4. Run checks appropriate to the change. List commands, results and skipped checks in the PR, distinguishing mock upstreams and offline tests from live account verification.
5. Read the DCO section below, sign off your commits with your own public identity and push to your fork.
6. Open a PR against the default branch of `hulkbig/ferry`. Use Draft while work is incomplete, then mark it ready for review.

Use a title that describes the problem being solved. Explain the trigger, behavior before and after, validation and compatibility impact; link an issue if one exists. For UI changes, include redacted before-and-after screenshots with language, theme and viewport width. Do not upload real accounts, databases, backups or user conversations.

Maintainers may request a minimal reproduction, narrower scope or additional tests. Update the same PR in response to review. Passing CI does not mean a change has been merged or verified against live upstreams. There is no guaranteed review time.

## Select the relevant checks

```sh
# Example: management API changes
go test ./internal/management

# Console behavior
npm test --prefix web/console

# Full product checks
bash scripts/test.sh
```

For documentation changes, check commands, links and both languages. Protocol, authorization, migration and concurrency changes need tests for the affected failure paths; run relevant race checks for concurrent code. See [development](docs/development.en.md) for prerequisites and commands.

## Provider and compatibility changes

Start with a versioned official protocol or client source and a redacted reproduction. Third-party projects can supply compatibility clues, but do not replace independent verification. Identify changes to authentication, translation and provider-specific behavior. Explain which fields are preserved, degraded or rejected. Add relevant regression cases for stream termination, tool-call IDs, cancellation and errors. See [provider adapter boundaries](docs/provider-adapters.md) (Chinese).

## Provenance and sign-off

Original contributions use Apache-2.0. When copying or adapting third-party code, identify the project, exact version, license and modification scope, and retain its notices. Fonts, icons and other assets need provenance too.

Contributions use the [Developer Certificate of Origin 1.1](https://developercertificate.org/). Read it and use the following only if you can make its certifications:

```sh
git commit -s
```

Use a name and email you are willing to make public in `Signed-off-by`; do not sign for someone else. Contributors remain responsible for understanding, checking and establishing the provenance of AI-assisted changes. See [NOTICE](NOTICE) and [LICENSES](LICENSES) for third-party material.

## Working together

Discuss code, problems and evidence while respecting different experience and opinions. Do not harass, attack people or publish private information. Maintainers may close duplicate, unverifiable or out-of-scope requests and explain why.
