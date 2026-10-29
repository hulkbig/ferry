#!/usr/bin/env bash
set -euo pipefail

readonly TRIVY_VERSION="0.74.0"
readonly TRIVY_RELEASE="v${TRIVY_VERSION}"
readonly POSITIVE_CONTROL_IMAGE="debian@sha256:e3ed4be20c22a1358020358331d177aa2860632f25b21681d79204ace20455a6"

if (( $# < 1 || $# > 2 )); then
  echo "usage: check_container_vulnerabilities.sh <image-ref> [docker|remote]" >&2
  exit 64
fi
image_ref=$1
image_source=${2:-docker}
case "$image_source" in
  docker|remote) ;;
  *) echo "image source must be docker or remote" >&2; exit 64 ;;
esac

case "$(uname -s):$(uname -m)" in
  Darwin:arm64)
    asset="trivy_${TRIVY_VERSION}_macOS-ARM64.tar.gz"
    asset_sha256="1caada5e0e2091909357c7525d3aa76f4b660b13821bc143b190c7483e31cc11"
    ;;
  Darwin:x86_64)
    asset="trivy_${TRIVY_VERSION}_macOS-64bit.tar.gz"
    asset_sha256="472816f6888dda689d075c30254d4210b4d1035acf365aa72332f584c2f60485"
    ;;
  Linux:x86_64)
    asset="trivy_${TRIVY_VERSION}_Linux-64bit.tar.gz"
    asset_sha256="2ae6fe3ee734b7fdf11335663e18c75ea12dccc76062f09f164a3b0f8be4371a"
    ;;
  Linux:aarch64|Linux:arm64)
    asset="trivy_${TRIVY_VERSION}_Linux-ARM64.tar.gz"
    asset_sha256="b94ce1976bbf3c15b514b605ee88be7c6d94a29be2302847ff01cb794d47aad5"
    ;;
  *) echo "unsupported platform for pinned Trivy release" >&2; exit 1 ;;
esac

scan_tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/ferry-trivy.XXXXXX")"
case "$scan_tmp_dir" in
  "${TMPDIR:-/tmp}"/ferry-trivy.*) ;;
  *) echo "unexpected Trivy temporary directory: $scan_tmp_dir" >&2; exit 1 ;;
esac
cleanup() {
  if [[ -d "$scan_tmp_dir" && ! -L "$scan_tmp_dir" ]]; then
    find "$scan_tmp_dir" -depth -delete
  fi
}
trap cleanup EXIT
chmod 0700 "$scan_tmp_dir"

archive="$scan_tmp_dir/$asset"
curl -fsSL -o "$archive" \
  "https://github.com/aquasecurity/trivy/releases/download/${TRIVY_RELEASE}/${asset}"
if command -v sha256sum >/dev/null 2>&1; then
  actual_sha256="$(sha256sum "$archive" | awk '{print $1}')"
else
  actual_sha256="$(shasum -a 256 "$archive" | awk '{print $1}')"
fi
[[ "$actual_sha256" == "$asset_sha256" ]] || {
  echo "Trivy release checksum mismatch" >&2
  exit 1
}
tar -xzf "$archive" -C "$scan_tmp_dir" trivy
chmod 0700 "$scan_tmp_dir/trivy"
version_output="$("$scan_tmp_dir/trivy" --version)"
grep -Fqx "Version: ${TRIVY_VERSION}" <<<"$version_output" || {
  printf '%s\n' "$version_output" >&2
  echo "unexpected Trivy version" >&2
  exit 1
}

if [[ -n "${FERRY_TRIVY_CACHE_DIR:-}" ]]; then
  cache_dir=$FERRY_TRIVY_CACHE_DIR
  install -d -m 0700 "$cache_dir"
else
  cache_dir="$scan_tmp_dir/cache"
  install -d -m 0700 "$cache_dir"
fi

"$scan_tmp_dir/trivy" image \
  --disable-telemetry \
  --cache-dir "$cache_dir" \
  --image-src remote \
  --scanners vuln \
  --pkg-types os,library \
  --timeout 10m \
  --format json \
  --output "$scan_tmp_dir/positive-control.json" \
  "$POSITIVE_CONTROL_IMAGE" \
  >"$scan_tmp_dir/positive-control.stdout" \
  2>"$scan_tmp_dir/positive-control.stderr"

python3 - "$scan_tmp_dir/positive-control.json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    report = json.load(handle)
issues = [
    issue
    for result in report.get("Results") or []
    for issue in result.get("Vulnerabilities") or []
    if issue.get("Severity") in {"HIGH", "CRITICAL"}
]
if not issues:
    raise SystemExit("Trivy positive control did not find a high/critical vulnerability")
print(f"Trivy positive control verified: high_or_critical={len(issues)}")
PY

"$scan_tmp_dir/trivy" image \
  --disable-telemetry \
  --cache-dir "$cache_dir" \
  --skip-db-update \
  --image-src "$image_source" \
  --scanners vuln \
  --pkg-types os,library \
  --timeout 10m \
  --format json \
  --output "$scan_tmp_dir/target.json" \
  "$image_ref" \
  >"$scan_tmp_dir/target.stdout" \
  2>"$scan_tmp_dir/target.stderr"

python3 - "$scan_tmp_dir/target.json" "$cache_dir/db/metadata.json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    report = json.load(handle)
results = report.get("Results") or []
if report.get("ArtifactType") != "container_image" or not results:
    raise SystemExit("Trivy did not identify a scannable container image")
issues = [
    issue
    for result in results
    for issue in result.get("Vulnerabilities") or []
]
blocking = [issue for issue in issues if issue.get("Severity") in {"HIGH", "CRITICAL"}]
counts = {}
for issue in issues:
    severity = issue.get("Severity") or "UNKNOWN"
    counts[severity] = counts.get(severity, 0) + 1
with open(sys.argv[2], encoding="utf-8") as handle:
    database = json.load(handle)
print(
    "container vulnerability summary: "
    f"high={counts.get('HIGH', 0)} critical={counts.get('CRITICAL', 0)} "
    f"medium={counts.get('MEDIUM', 0)} low={counts.get('LOW', 0)} "
    f"unknown={counts.get('UNKNOWN', 0)} db_updated_at={database.get('UpdatedAt')}"
)
if blocking:
    for issue in blocking:
        print(
            f"blocking vulnerability id={issue.get('VulnerabilityID')} "
            f"package={issue.get('PkgName')} severity={issue.get('Severity')}",
            file=sys.stderr,
        )
    raise SystemExit("container image has high/critical vulnerabilities")
PY

echo "Container vulnerability policy verified with Trivy ${TRIVY_VERSION}"
