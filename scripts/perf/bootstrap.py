"""Run inside the isolated lab's controller; initializes synthetic test accounts only."""

import json, urllib.request, urllib.error, http.cookiejar, time, pathlib

cookies = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cookies))


def api(host, path, body=None, token=None, method=None):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(
        "http://" + host + path,
        data=None if body is None else json.dumps(body).encode(),
        headers=headers,
        method=method,
    )
    try:
        with opener.open(req, timeout=30) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        raise RuntimeError(f"{host}{path} {e.code} {e.read()[:500]!r}")


for n in range(90):
    try:
        api("sub2api:8080", "/health")
        break
    except Exception:
        time.sleep(1)
else:
    raise RuntimeError("sub2api not ready")
login = api(
    "sub2api:8080",
    "/api/v1/auth/login",
    {"email": "bench@example.test", "password": "Bench-Local-Only-2026!"},
)["data"]
token = login["access_token"]
uid = login["user"]["id"]
# Explicit user authorization was received for this isolated test instance only.
if "--accept-compliance" in __import__("sys").argv:
    api(
        "sub2api:8080",
        "/api/v1/admin/compliance/accept",
        {"phrase": "我已阅读、理解并同意 Sub2API 部署与运营合规承诺", "language": "zh"},
        token,
    )
api(
    "sub2api:8080",
    f"/api/v1/admin/users/{uid}",
    {"concurrency": 1024, "balance": 1000000},
    token,
    "PUT",
)
g = api(
    "sub2api:8080",
    "/api/v1/admin/groups",
    {
        "name": "benchmark-openai",
        "platform": "openai",
        "rate_multiplier": 1,
        "subscription_type": "standard",
    },
    token,
)["data"]
a = api(
    "sub2api:8080",
    "/api/v1/admin/accounts",
    {
        "name": "benchmark-mock",
        "platform": "openai",
        "type": "apikey",
        "credentials": {
            "api_key": "mock-only",
            "base_url": "http://mock:8080",
            "model_mapping": {"gpt-4o-mini": "gpt-4o-mini"},
        },
        "concurrency": 1024,
        "priority": 1,
        "group_ids": [g["id"]],
    },
    token,
)["data"]
k = api(
    "sub2api:8080",
    "/api/v1/keys",
    {
        "name": "benchmark",
        "group_id": g["id"],
        "custom_key": "sk-benchmark-local-only-20261030",
    },
    token,
)["data"]
# Keys are synthetic and valid only on this isolated network.
pathlib.Path("/results/lab-keys.json").write_text(
    json.dumps(
        {
            "ferry": "bench-only",
            "ferry-opt": "bench-only",
            "cliproxy": "bench-only",
            "sub2api": k["key"],
            "direct": "bench-only",
        }
    )
)
status = api("ferry:7900", "/api/auth/status")
if not status.get("initialized"):
    api(
        "ferry:7900",
        "/api/auth/initialize",
        {"username": "bench", "password": "Bench-Local-Only-2026!"},
    )
print(
    json.dumps(
        {
            "sub2api_group": g["id"],
            "sub2api_account": a["id"],
            "sub2api_key_created": True,
            "ferry_initialized": True,
        }
    )
)
for name, host, key in [
    ("ferry", "ferry:7890", "bench-only"),
    ("cliproxy", "cliproxy:8317", "bench-only"),
    ("sub2api", "sub2api:8080", k["key"]),
]:
    for stream in [False]:
        v = api(
            host,
            "/v1/chat/completions",
            {
                "model": "gpt-4o-mini",
                "messages": [{"role": "user", "content": "hello"}],
                "stream": stream,
            },
            key,
        )
        print(name, "content_bytes", len(v["choices"][0]["message"]["content"]))
