#!/usr/bin/env bash
set -euo pipefail

data_url=""
management_url=""
expected_commit=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --data-url) data_url="${2:?missing --data-url value}"; shift 2 ;;
    --management-url) management_url="${2:?missing --management-url value}"; shift 2 ;;
    --expected-commit) expected_commit="${2:?missing --expected-commit value}"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[[ -n "$data_url" ]] || { echo "--data-url is required" >&2; exit 2; }
[[ -n "$management_url" ]] || { echo "--management-url is required" >&2; exit 2; }
[[ "$expected_commit" =~ ^[0-9a-f]{40}$ ]] || { echo "--expected-commit must be a full Git SHA" >&2; exit 2; }
data_url="${data_url%/}"
management_url="${management_url%/}"
[[ "$data_url" == "$management_url" ]] || {
  echo "data and management URLs must use one canonical domain" >&2
  exit 2
}

version_json=$(curl --fail --silent --show-error --proto '=https' --tlsv1.2 "$data_url/version")
actual_commit=$(VERSION_JSON="$version_json" python3 -c 'import json,os; print(json.loads(os.environ["VERSION_JSON"]).get("commit",""))')
[[ "$actual_commit" == "$expected_commit" ]] || {
  echo "version commit mismatch: expected=$expected_commit actual=$actual_commit" >&2
  exit 1
}
curl --fail --silent --show-error --proto '=https' --tlsv1.2 "$data_url/health" >/dev/null

management_headers=$(curl --silent --output /dev/null --dump-header - --write-out $'\nHTTP_STATUS:%{http_code}\n' \
  --proto '=https' --tlsv1.2 "$management_url/")
management_status=$(sed -n 's/^HTTP_STATUS://p' <<<"$management_headers")
[[ "$management_status" == 200 ]] || {
  echo "management anonymous root = HTTP $management_status, want 200" >&2
  exit 1
}
if grep -Eiq '^www-authenticate:[[:space:]]*basic' <<<"$management_headers"; then
  echo "management root unexpectedly returned a Basic authentication challenge" >&2
  exit 1
fi
console_html=$(curl --fail --silent --show-error --proto '=https' --tlsv1.2 "$management_url/")
grep -Fq '<title>Ferry Console</title>' <<<"$console_html" || {
  echo "management root did not serve the Ferry console" >&2
  exit 1
}

auth_status_json=$(curl --fail --silent --show-error --proto '=https' --tlsv1.2 "$management_url/api/auth/status")
AUTH_STATUS_JSON="$auth_status_json" python3 -c 'import json,os; d=json.loads(os.environ["AUTH_STATUS_JSON"]); assert isinstance(d.get("initialized"),bool); assert isinstance(d.get("authenticated"),bool)'

management_api_headers=$(curl --silent --output /dev/null --dump-header - --write-out $'\nHTTP_STATUS:%{http_code}\n' \
  --proto '=https' --tlsv1.2 "$management_url/api/overview")
management_api_status=$(sed -n 's/^HTTP_STATUS://p' <<<"$management_api_headers")
[[ "$management_api_status" == 401 || "$management_api_status" == 428 ]] || {
  echo "management protected API = HTTP $management_api_status, want 401 or 428" >&2
  exit 1
}
if grep -Eiq '^www-authenticate:[[:space:]]*basic' <<<"$management_api_headers"; then
  echo "management API unexpectedly returned a Basic authentication challenge" >&2
  exit 1
fi

probe_body='{"model":"ferry-route-probe","messages":[{"role":"user","content":"route probe"}],"stream":false}'
data_headers=$(curl --silent --output /dev/null --dump-header - --write-out $'\nHTTP_STATUS:%{http_code}\n' \
  --proto '=https' --tlsv1.2 --header 'Content-Type: application/json' \
  --data "$probe_body" "$data_url/v1/chat/completions")
data_status=$(sed -n 's/^HTTP_STATUS://p' <<<"$data_headers")
[[ "$data_status" == 401 ]] || {
  echo "unauthenticated data route = HTTP $data_status, want Ferry 401" >&2
  exit 1
}
if grep -Eiq '^www-authenticate:[[:space:]]*basic' <<<"$data_headers"; then
  echo "data route was intercepted by proxy Basic Auth" >&2
  exit 1
fi

basic_headers=$(curl --silent --output /dev/null --dump-header - --write-out $'\nHTTP_STATUS:%{http_code}\n' \
  --proto '=https' --tlsv1.2 --user 'ferry-invalid:ferry-invalid' \
  --header 'Content-Type: application/json' --data "$probe_body" \
  "$data_url/v1/chat/completions")
basic_status=$(sed -n 's/^HTTP_STATUS://p' <<<"$basic_headers")
[[ "$basic_status" == 401 ]] || {
  echo "Basic credentials unexpectedly authorized data route: HTTP $basic_status" >&2
  exit 1
}
if grep -Eiq '^www-authenticate:[[:space:]]*basic' <<<"$basic_headers"; then
  echo "data route inherited the management Basic challenge" >&2
  exit 1
fi

printf 'deployment verified: commit=%s data=%s management=%s routing=single-domain management-auth=built-in-session+management-token\n' \
  "$expected_commit" "$data_url" "$management_url"
