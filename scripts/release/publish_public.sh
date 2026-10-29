#!/usr/bin/env bash
# Called only by Public release after both native-platform jobs pass.
set -euo pipefail
checked=${1:?checked artifact directory required}
: "${VERSION:?}" "${GITHUB_SHA:?}" "${GITHUB_REPOSITORY:?}" "${DOCKERHUB_TOKEN:?}"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
[[ "$GITHUB_REPOSITORY" == hulkbig/ferry && "$GITHUB_REF" == refs/heads/main ]]
[[ "$(git rev-parse HEAD)" == "$GITHUB_SHA" ]]
test -f "docs/releases/$VERSION.md"
window() { python3 scripts/release/public_window.py --minimum-remaining-minutes 30; }
window
gh api "repos/$GITHUB_REPOSITORY" --jq '.private == false' | grep -qx true

# A release must refer to a successful full Product checks run at this exact SHA.
gh api "repos/$GITHUB_REPOSITORY/actions/workflows/test.yml/runs?head_sha=$GITHUB_SHA&status=success&per_page=100" \
  --jq '.workflow_runs | any(.head_branch == "main" and .event != "pull_request" and .conclusion == "success")' | grep -qx true

# Refuse a partial prior release: reconcile its state instead of replacing tags.
python3 - "$VERSION" <<'PY'
import sys, urllib.request, urllib.error
version = sys.argv[1]
for tag in (version, version+'-amd64', version+'-arm64'):
    try:
        urllib.request.urlopen('https://hub.docker.com/v2/namespaces/bighulk/repositories/ferry/tags/'+tag, timeout=30).close()
    except urllib.error.HTTPError as error:
        if error.code == 404:
            continue
        raise SystemExit('Cannot verify tag absence; publication stopped.')
    raise SystemExit('Tag already exists; reconcile the previous release before retrying.')
PY
python3 - <<'PY'
import os,subprocess
result=subprocess.run(['gh','api','repos/'+os.environ['GITHUB_REPOSITORY']+'/git/ref/tags/v'+os.environ['VERSION']],capture_output=True,text=True)
if result.returncode == 0:
    raise SystemExit('Git version tag already exists; publication stopped.')
if 'HTTP 404' not in result.stderr:
    raise SystemExit('Cannot verify Git tag absence; publication stopped.')
PY
for arch in amd64 arm64; do
  [[ "$(cat "$checked/source-$arch.txt")" == "$GITHUB_SHA" ]]
  docker load -i "$checked/image-$arch.tar"
  [[ "$(docker image inspect "ferry:release-$arch" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}')" == "$GITHUB_SHA" ]]
  [[ "$(docker image inspect "ferry:release-$arch" --format '{{.Architecture}}')" == "$arch" ]]
done

docker_config=$(mktemp -d)
chmod 700 "$docker_config"
cleanup() { rm -rf "$docker_config"; }
trap cleanup EXIT
printf '%s' "$DOCKERHUB_TOKEN" | docker --config "$docker_config" login -u bighulk --password-stdin
for arch in amd64 arm64; do
  docker tag "ferry:release-$arch" "bighulk/ferry:$VERSION-$arch"
  window
  docker --config "$docker_config" push "bighulk/ferry:$VERSION-$arch"
done
window
DOCKER_CONFIG="$docker_config" docker buildx imagetools create -t "bighulk/ferry:$VERSION" \
  "bighulk/ferry:$VERSION-amd64" "bighulk/ferry:$VERSION-arm64"

# Confirm anonymous pulls, independently of the authenticated Docker configuration.
anonymous_config=$(mktemp -d "$docker_config/anonymous.XXXXXX")
for arch in amd64 arm64; do
  docker --config "$anonymous_config" pull --platform "linux/$arch" "bighulk/ferry:$VERSION"
done
docker buildx imagetools inspect "bighulk/ferry:$VERSION" --raw > "$checked/manifest.json"
python3 - "$checked/manifest.json" <<'PY'
import json,sys
m=json.load(open(sys.argv[1]))
assert {(p['platform']['os'],p['platform']['architecture']) for p in m['manifests']} == {('linux','amd64'),('linux','arm64')}
PY
window
DOCKER_CONFIG="$docker_config" docker buildx imagetools create -t bighulk/ferry:latest "bighulk/ferry:$VERSION"
cp compose.yaml "$checked/compose.yaml"
(cd "$checked" && sha256sum compose.yaml sbom-*.cdx.json manifest.json > SHA256SUMS)
window
gh release create "v$VERSION" "$checked/compose.yaml" "$checked/SHA256SUMS" \
  "$checked/manifest.json" "$checked/"sbom-*.cdx.json \
  --repo "$GITHUB_REPOSITORY" --target "$GITHUB_SHA" --draft \
  --title "Ferry $VERSION" --notes-file "docs/releases/$VERSION.md"
window
gh release edit "v$VERSION" --repo "$GITHUB_REPOSITORY" --draft=false --latest
