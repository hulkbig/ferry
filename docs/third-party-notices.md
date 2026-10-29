# Runtime dependency notices

The command dependency graph was inspected with `go list -deps ./cmd/ferry`.
The following exact-version module root license/notice files are preserved verbatim
in `LICENSES/dependencies`. Checksums and paths are in [the inventory](runtime-dependencies.json).
This inventory covers runtime modules, not the complete build/test toolchain,
container trust store, or unresolved first-party source provenance. It is not a
claim that all release licensing work is complete.

Go's root LICENSE and PATENTS are preserved in `LICENSES/toolchain`, with the
toolchain version and hashes recorded in `toolchain-notices.json`.
`python3 scripts/check_notices.py` checks these bytes against the active Go
toolchain and checks the runtime module inventory against the union of
`darwin/arm64`, `linux/amd64` and `linux/arm64` dependency graphs with CGO disabled.
The portable product gate runs this check to catch dependency or toolchain drift.
The check also compares notices in each used package's ancestor directories with
the exact module cache bytes, including PATENTS and the embedded netdb component.
Go's time-data README is preserved with the toolchain notices. Container materials
and remaining CA bundle review are described in [container notices](container-notices.md).
This does not establish complete provenance or detect notices embedded only in
source comments or files with unrelated names.

- `github.com/dustin/go-humanize@v1.0.1`
- `github.com/google/uuid@v1.6.0`
- `github.com/mattn/go-isatty@v0.0.20`
- `github.com/ncruces/go-strftime@v0.1.9`
- `github.com/remyoudompheng/bigfft@v0.0.0-20230129092748-24d4a6f8daec`
- `golang.org/x/crypto@v0.56.0`
- `golang.org/x/exp@v0.0.0-20250408133849-7e4ce0ab07d0`
- `golang.org/x/sys@v0.47.0`
- `gopkg.in/yaml.v3@v3.0.1`
- `modernc.org/libc@v1.65.10`
- `modernc.org/mathutil@v1.7.1`
- `modernc.org/memory@v1.11.0`
- `modernc.org/sqlite@v1.38.0`
