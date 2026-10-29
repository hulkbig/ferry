#!/usr/bin/env bash
set -euo pipefail

readonly GOVULNCHECK_MODULE="golang.org/x/vuln/cmd/govulncheck"
readonly GOVULNCHECK_VERSION="v1.7.0"

tool_dir="$(mktemp -d "${TMPDIR:-/tmp}/ferry-govulncheck.XXXXXX")"
trap 'rm -rf -- "$tool_dir"' EXIT
install -d -m 0700 "$tool_dir/bin"

GOBIN="$tool_dir/bin" GOTOOLCHAIN=local \
  go install "${GOVULNCHECK_MODULE}@${GOVULNCHECK_VERSION}"

scanner="$tool_dir/bin/govulncheck"
version_output="$($scanner -version)"
grep -Fq "Scanner: govulncheck@${GOVULNCHECK_VERSION}" <<<"$version_output" || {
  printf '%s\n' "$version_output" >&2
  echo "unexpected govulncheck version" >&2
  exit 1
}

echo "scanning production Go call graph with govulncheck ${GOVULNCHECK_VERSION}"
"$scanner" ./...

echo "scanning test-inclusive Go call graph with govulncheck ${GOVULNCHECK_VERSION}"
"$scanner" -test ./...

echo "Go vulnerability policy verified"
