#!/usr/bin/env python3
"""Exercise an installed image using disposable volumes and loopback ports.

No source mount, upstream credentials, registry login, or production data required.
This checks installation and recovery; it does not certify a real provider.
"""
import argparse
import http.client
import json
import secrets
import subprocess
import time
import uuid


def docker(*args):
    result = subprocess.run(["docker", *args], capture_output=True, text=True, timeout=180)
    if result.returncode:
        # Never echo command arguments or container output, which can contain secrets.
        raise RuntimeError("Docker operation failed: " + args[0])
    return result.stdout.strip()


def request(port, path, method="GET", body=None, cookies="", expected=200, bearer=""):
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=15)
    headers = {"Content-Type": "application/json", "Cookie": cookies}
    if bearer:
        headers["Authorization"] = "Bearer " + bearer
    for cookie in cookies.split("; "):
        if cookie.startswith("smartapi_admin_csrf="):
            headers["X-CSRF-Token"] = cookie.split("=", 1)[1]
    connection.request(method, path, json.dumps(body) if body is not None else None, headers)
    response = connection.getresponse()
    payload = response.read()
    cookie = "; ".join(value.split(";", 1)[0] for key, value in response.getheaders()
                       if key.lower() == "set-cookie")
    status = response.status
    connection.close()
    if status != expected:
        raise RuntimeError(f"{method} {path}: expected {expected}, got {status}")
    if not payload or not payload.lstrip().startswith((b"{", b"[")):
        return payload, cookie
    return json.loads(payload), cookie


def wait_ready(name):
    ports = json.loads(docker("inspect", "--format", "{{json .NetworkSettings.Ports}}", name))
    management = int(ports["7900/tcp"][0]["HostPort"])
    inference = int(ports["7890/tcp"][0]["HostPort"])
    for _ in range(90):
        try:
            request(inference, "/health")
            return management, inference
        except (OSError, http.client.HTTPException, RuntimeError):
            time.sleep(1)
    raise RuntimeError("container did not become ready")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("image")
    parser.add_argument("--platform", choices=["linux/amd64", "linux/arm64"], required=True)
    args = parser.parse_args()
    prefix = "ferry-smoke-" + uuid.uuid4().hex[:12]
    volumes = [prefix + "-data", prefix + "-configuration", prefix + "-full"]
    containers = [prefix, prefix + "-configuration", prefix + "-full"]
    password = secrets.token_urlsafe(32)
    common = ["--platform", args.platform, "--read-only", "--cap-drop", "ALL",
              "--security-opt", "no-new-privileges:true", "--tmpfs", "/tmp:size=32m,mode=1777"]

    def start(name, volume):
        docker("run", "-d", "--name", name, *common,
               "-p", "127.0.0.1::7900", "-p", "127.0.0.1::7890",
               "-v", volume + ":/data", args.image)
        return wait_ready(name)

    def login(port):
        result, cookie = request(port, "/api/auth/login", "POST",
                                 {"username": "smoke-admin", "password": password})
        assert result["authenticated"]
        request(port, "/api/overview", cookies=cookie)
        return cookie

    try:
        for volume in volumes:
            docker("volume", "create", volume)
        management, inference = start(containers[0], volumes[0])
        status, _ = request(management, "/api/auth/status")
        assert not status["initialized"]
        request(management, "/api/overview", expected=428)
        request(inference, "/v1/models", expected=401)
        page, _ = request(management, "/")
        assert b"html" in page.lower()
        initialized, cookie = request(management, "/api/auth/initialize", "POST",
                                      {"username": "smoke-admin", "password": password}, expected=201)
        assert initialized["initialized"] and cookie
        request(management, "/api/overview", cookies=cookie)
        request(management, "/api/overview", expected=401)
        key, _ = request(management, "/api/api-keys", "POST", {"name": "smoke-client"},
                         cookies=cookie, expected=201)
        client_key = key["secret"]
        request(inference, "/v1/models", bearer=client_key)
        # An authenticated rejected request creates real ledger history without
        # an upstream account, source mount, fixture database or paid inference.
        request(inference, "/v1/chat/completions", "POST",
                {"model": "smoke-missing-model", "messages": [{"role": "user", "content": "smoke"}]},
                expected=422, bearer=client_key)
        docker("restart", containers[0])
        management, inference = wait_ready(containers[0])
        cookie = login(management)
        history, _ = request(management, "/api/requests", cookies=cookie)
        history_ids = {row["id"] for row in history["requests"]}
        assert history_ids and history["total"] == len(history_ids)
        for scope, flags in (("configuration", []), ("full", ["-backup-history"])):
            manifest = json.loads(docker("exec", containers[0], "/usr/local/bin/ferry",
                                        "-data-dir", "/data/state", "-backup-to",
                                        "/data/backups/" + scope, *flags))
            # Legacy full-backup manifests omit scope; keep that wire format.
            assert manifest.get("scope", "full") == scope
            docker("exec", containers[0], "/usr/local/bin/ferry", "-verify-backup",
                   "/data/backups/" + scope)
        docker("stop", containers[0])
        for index, scope in enumerate(("configuration", "full"), 1):
            docker("run", "--rm", *common, "--network", "none",
                   "-v", volumes[0] + ":/source:ro", "-v", volumes[index] + ":/data",
                   args.image, "-data-dir", "/data/state", "-restore-from", "/source/backups/" + scope)
            management, inference = start(containers[index], volumes[index])
            cookie = login(management)
            request(inference, "/v1/models", expected=401)
            request(inference, "/v1/models", bearer=client_key)
            restored, _ = request(management, "/api/requests", cookies=cookie)
            expected_ids = history_ids if scope == "full" else set()
            assert {row["id"] for row in restored["requests"]} == expected_ids
            assert restored["total"] == len(expected_ids)
            version, _ = request(inference, "/version")
            docker("stop", containers[index])
        print(json.dumps({"platform": args.platform, "version": version,
                          "checks": ["empty-install", "console", "authentication", "restart-persistence",
                                     "authenticated-rejected-request-history", "online-backup",
                                     "backup-verification", "restore-and-login", "restored-client-key",
                                     "configuration-excludes-history", "explicit-full-retains-history"],
                          "real_upstream_tested": False,
                          "result": "passed"}))
    finally:
        for name in containers:
            subprocess.run(["docker", "rm", "-f", name], capture_output=True)
        for volume in volumes:
            subprocess.run(["docker", "volume", "rm", volume], capture_output=True)


if __name__ == "__main__":
    main()
