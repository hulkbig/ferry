"""Functional acceptance of the final headless binary on synthetic upstreams."""

import http.client
import json
import pathlib
import socket
import time
import urllib.error
import urllib.request


def api(host, path, body=None, key=None):
    headers = {"Content-Type": "application/json"}
    if key:
        headers["Authorization"] = "Bearer " + key
    req = urllib.request.Request(
        "http://" + host + path,
        data=None if body is None else json.dumps(body).encode(),
        headers=headers,
    )
    with urllib.request.urlopen(req, timeout=10) as response:
        return json.load(response)


checks = {}
checks["health"] = api("ferry-opt:7890", "/health")
try:
    with socket.create_connection(("ferry-opt", 7900), timeout=1):
        raise AssertionError("headless management port is listening")
except ConnectionRefusedError:
    checks["management_not_listening"] = True
try:
    api("ferry-opt:7890", "/v1/models")
    raise AssertionError("unauthenticated request allowed")
except urllib.error.HTTPError as error:
    assert error.code == 401
    checks["authentication_required"] = True
api(
    "mock:8080",
    "/config",
    {"delay_ms": 30, "jitter_ms": 10, "content_bytes": 65536, "chunks": 1},
)
start = time.monotonic()
response = api(
    "ferry-opt:7890",
    "/v1/chat/completions",
    {"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "acceptance"}]},
    "bench-only",
)
assert len(response["choices"][0]["message"]["content"]) == 65536
assert time.monotonic() - start >= 0.03
checks["delayed_large_response"] = True
before = api("mock:8080", "/stats")
api(
    "mock:8080",
    "/config",
    {"delay_ms": 20, "content_bytes": 16384, "chunks": 200, "interval_ms": 50},
)
client = http.client.HTTPConnection("ferry-opt", 7890, timeout=5)
client.request(
    "POST",
    "/v1/chat/completions",
    json.dumps(
        {
            "model": "gpt-4o-mini",
            "messages": [{"role": "user", "content": "cancel test"}],
            "stream": True,
        }
    ),
    {"Authorization": "Bearer bench-only", "Content-Type": "application/json"},
)
response = client.getresponse()
assert response.status == 200
response.read(1)
response.close()
client.close()
deadline = time.monotonic() + 5
while True:
    after = api("mock:8080", "/stats")
    if after["active"] == 0 and after["cancelled"] > before["cancelled"]:
        break
    assert time.monotonic() < deadline, "upstream did not observe cancellation"
    time.sleep(0.05)
checks["cancel_propagates_to_upstream"] = True
api("mock:8080", "/config", {"content_bytes": 1024, "chunks": 1})
response = api(
    "ferry-opt:7890",
    "/v1/chat/completions",
    {
        "model": "gpt-4o-mini",
        "messages": [{"role": "user", "content": "after cancellation"}],
    },
    "bench-only",
)
assert len(response["choices"][0]["message"]["content"]) == 1024
checks["request_after_cancel"] = True
# Fault injection is last so any gateway cooldown cannot contaminate throughput tests.
api("mock:8080", "/config", {"content_bytes": 1024, "chunks": 1, "status": 500})
try:
    api(
        "ferry-opt:7890",
        "/v1/chat/completions",
        {
            "model": "gpt-4o-mini",
            "messages": [{"role": "user", "content": "fault test"}],
        },
        "bench-only",
    )
    raise AssertionError("upstream error was returned as success")
except urllib.error.HTTPError as error:
    assert error.code in (500, 502, 503)
    checks["upstream_failure_not_success"] = error.code
finally:
    api("mock:8080", "/config", {"content_bytes": 1024, "chunks": 1})
pathlib.Path("/results/headless-acceptance.json").write_text(
    json.dumps(checks, indent=2)
)
print(json.dumps(checks))
