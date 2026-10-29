# Development

[中文](development.md) · [Contributing](../CONTRIBUTING.en.md)

## Environment and layout

Use the Go version declared in `go.mod`, currently Go 1.26.10. Full checks also require Node.js, npm and Python 3; CI uses Node.js 22 and Python 3.12. The console is plain JavaScript/CSS and does not require an npm package installation.

| Path | Purpose |
| --- | --- |
| `cmd/ferry/` | Entry point, startup and maintenance commands |
| `internal/` | Configuration, management, routing, protocols, accounts and storage |
| `web/console/` | Console source and behavior tests, embedded in the binary |
| `skills/ferry/` | Agent Skill, Python client and management API reference |
| `deploy/` | Dockerfile, source-build Compose and container configuration |
| `compose.yaml` | Version-pinned public image installation |
| `scripts/` | Checks, candidate builds and scans |
| `website/` | Static bilingual product website |
| `docs/guide/` | User guides |
| `docs/reference/native/` | Native account driver references |
| `docs/archive/` | Historical designs with stated limitations |
| `LICENSES/` | Third-party license texts and provenance |
| `.github/` | CI, issue forms and PR template |

Build scripts depend on the license inventory paths. Update packaging and checks when moving them. The Go module name is `ferry`; the executable entry is `cmd/ferry`.

## Request flow

```text
client → ingress → policy / scheduler → egress / nativewire → upstream
                           ↓
                         ledger

console / agent → management → controlstore / secretstore
native account lifecycle → nativeauth; Codex OAuth → oauth
```

`conversation` tracks continuation state; `runtime` manages runtime state; `billing` handles pricing and usage. Changes to public protocols or configuration need a review of ingress, egress, management, console and Skill callers.

## Run and check

```sh
go run ./cmd/ferry -data-dir ./data -config ./config.example.yaml
```

The console uses Go embed. Rebuild and restart your local instance after JS/CSS changes to inspect the actual served page.

```sh
# Example Go package
go test ./internal/management

# Console behavior and assembly
npm test --prefix web/console

# Python management client
python3 -B -m unittest discover -s skills/ferry/scripts -p 'test_*.py'

# Notices, Go test/vet/build, console, Skill and release-script tests
bash scripts/test.sh
```

Select checks by impact. Concurrent account, protocol or scheduling changes also need relevant race tests, for example:

```sh
go test -race ./internal/nativeauth ./internal/nativewire ./internal/management
```

State whether tests use synthetic data or mock upstreams. Live provider checks require a separate account, defined scope and recorded results; ordinary development checks do not trigger them automatically.

## Security checks and build artifacts

```sh
python3 scripts/security_scan.py
python3 scripts/check_source.py
bash scripts/check_go_vulnerabilities.sh
```

Docker scanner images are pinned by digest. They are fetched before offline scanning. The secret scanner supports `--offline` when its image is already present and fails if it is missing. Focused static rules do not constitute a full security audit.

Build artifacts outside the source tree. See [candidate builds](local-candidate-builds.md) and [release checks](release-readiness.md), currently in Chinese, for platform binaries, notices, checksums, SBOMs and release evidence.

## Documentation

- Start with the reader's task, then give steps and conditions.
- Verify commands, ports, fields and capabilities against code, configuration and tests.
- Distinguish implementation, offline testing, live-account verification and production acceptance.
- Do not invent performance figures, compatibility rates, user counts or release dates.
- Keep debugging transcripts and private environments out of user guides; preserve provenance and mark historical designs.
- Keep paired Chinese/English documents aligned. Detailed references may remain in their original language when the index identifies it.

Contribution and provenance requirements are in [Contributing](../CONTRIBUTING.en.md).
