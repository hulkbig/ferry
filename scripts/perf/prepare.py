"""Generate an isolated Docker lab. All credentials below are synthetic, local-only."""

import json, pathlib, sys, copy

out = pathlib.Path(sys.argv[1]).resolve()
out.mkdir(parents=True, exist_ok=True)
(out / "ferry.yaml").write_text("""server:
  data_dir: /data
  management_addr: 0.0.0.0:7900
  proxy_addr: 0.0.0.0:7890
  timezone: UTC
consumer_keys:
  - name: bench
    key: bench-only
endpoints:
  - id: mock
    base_url: http://mock:8080/v1
    protocol: openai-chat
plans:
  - id: free
    kind: free
resource_targets:
  - id: mock-target
    endpoint_id: mock
    model: gpt-4o-mini
    plan_id: free
    credential: {secret_ref: env:MOCK_API_KEY}
    capabilities: {text: true, stream: true, context_window: 1048576}
pools:
  - id: pool
    targets: [mock-target]
policies:
  basic:
    tiers:
      - {name: main, pool: pool}
default_policy: basic
""")
(out / "cliproxy.yaml").write_text("""host: 0.0.0.0
port: 8317
auth-dir: /data/auth
api-keys: [bench-only]
commercial-mode: true
remote-management:
  secret-key: ""
  disable-control-panel: true
  disable-auto-update-panel: true
request-retry: 0
logging-to-file: false
usage-statistics-enabled: false
openai-compatibility:
  - name: mock
    base-url: http://mock:8080/v1
    api-key-entries:
      - api-key: mock-only
    models:
      - name: gpt-4o-mini
        alias: gpt-4o-mini
""")
base = {
    "platform": "linux/arm64",
    "cpus": 2,
    "mem_limit": "1536m",
    "networks": ["lab"],
    "restart": "no",
    "logging": {"driver": "local", "options": {"max-size": "10m", "max-file": "2"}},
}
services = {}


def add(n, **kw):
    services[n] = {**base, "container_name": "ferry-perf-" + n, **kw}


add(
    "controller",
    image="python:3.12-alpine",
    command=["sleep", "infinity"],
    volumes=[
        str(out) + ":/results",
        str(pathlib.Path(__file__).parent.resolve()) + ":/scripts:ro",
    ],
    cpus=0.25,
    mem_limit="128m",
)
image = "bighulk/ferry:1.0.0"
add(
    "mock",
    image=image,
    entrypoint=["/lab/perf-lab"],
    command=["mock"],
    volumes=[str(out / "bin/perf-lab") + ":/lab/perf-lab:ro"],
    ports=["127.0.0.1:19180:8080"],
)
add(
    "load",
    image=image,
    entrypoint=["/lab/perf-lab"],
    command=["load"],
    profiles=["tools"],
    volumes=[str(out / "bin/perf-lab") + ":/lab/perf-lab:ro", str(out) + ":/results"],
)
add(
    "ferry",
    image=image,
    command=["-data-dir", "/data", "-config", "/lab/ferry.yaml"],
    environment={"MOCK_API_KEY": "mock-only", "GOMAXPROCS": "2"},
    volumes=["ferry-data:/data", str(out / "ferry.yaml") + ":/lab/ferry.yaml:ro"],
    ports=["127.0.0.1:19190:7900", "127.0.0.1:19189:7890"],
)
add(
    "cliproxy",
    image="debian:bookworm-slim",
    user="0:0",
    entrypoint=["/lab/cli-proxy-api"],
    command=["-config", "/lab/config.yaml", "-local-model"],
    environment={"GOMAXPROCS": "2"},
    volumes=[
        str(out / "bin/cliproxy/cli-proxy-api") + ":/lab/cli-proxy-api:ro",
        str(out / "cliproxy.yaml") + ":/lab/config.yaml:ro",
        "cliproxy-data:/data",
    ],
    ports=["127.0.0.1:19187:8317"],
)
add(
    "postgres",
    image="postgres:17.6-alpine",
    environment={
        "POSTGRES_USER": "bench",
        "POSTGRES_PASSWORD": "bench-local-password",
        "POSTGRES_DB": "bench",
    },
    volumes=["postgres-data:/var/lib/postgresql/data"],
    cpus=1,
    mem_limit="768m",
)
add("redis", image="redis:7-alpine", cpus=1, mem_limit="256m")
add(
    "sub2api",
    image="weishaw/sub2api:0.2.19",
    environment={
        "AUTO_SETUP": "true",
        "SERVER_HOST": "0.0.0.0",
        "SERVER_PORT": "8080",
        "SERVER_MODE": "release",
        "RUN_MODE": "simple",
        "DATABASE_HOST": "postgres",
        "DATABASE_PORT": "5432",
        "DATABASE_USER": "bench",
        "DATABASE_PASSWORD": "bench-local-password",
        "DATABASE_DBNAME": "bench",
        "DATABASE_SSLMODE": "disable",
        "REDIS_HOST": "redis",
        "REDIS_PORT": "6379",
        "ADMIN_EMAIL": "bench@example.test",
        "ADMIN_PASSWORD": "Bench-Local-Only-2026!",
        "JWT_SECRET": "synthetic-benchmark-jwt-secret-not-for-real-use-2026",
        "TOTP_ENCRYPTION_KEY": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
        "SECURITY_URL_ALLOWLIST_ENABLED": "false",
        "SECURITY_URL_ALLOWLIST_ALLOW_INSECURE_HTTP": "true",
        "SECURITY_URL_ALLOWLIST_ALLOW_PRIVATE_HOSTS": "true",
        "GOMAXPROCS": "2",
    },
    volumes=["sub2api-data:/app/data"],
    ports=["127.0.0.1:19188:8080"],
    depends_on=["postgres", "redis"],
)
for n in ["ferry", "cliproxy", "sub2api", "mock", "load"]:
    services[n]["sysctls"] = {
        "net.ipv4.ip_local_port_range": "1024 65535",
        "net.ipv4.tcp_tw_reuse": "1",
    }
for n in ["mock", "load"]:
    services[n]["healthcheck"] = {"disable": True}
if len(sys.argv) > 2:
    candidate = pathlib.Path(sys.argv[2]).resolve()
    opt = copy.deepcopy(services["ferry"])
    opt["container_name"] = "ferry-perf-ferry-opt"
    opt["entrypoint"] = ["/lab/ferry-optimized"]
    opt["ports"] = []
    opt["volumes"] = [
        v.replace("ferry-data:/data", "ferry-opt-data:/data") for v in opt["volumes"]
    ] + [str(candidate) + ":/lab/ferry-optimized:ro"]
    services["ferry-opt"] = opt
    services["controller"]["volumes"] += [
        "ferry-data:/ferry-data:ro",
        "ferry-opt-data:/ferry-opt-data:ro",
    ]
(out / "compose.json").write_text(
    json.dumps(
        {
            "name": "ferry-perf",
            "services": services,
            "networks": {"lab": {"internal": True}},
            "volumes": {
                n: {}
                for n in [
                    "ferry-data",
                    "cliproxy-data",
                    "postgres-data",
                    "sub2api-data",
                    "ferry-opt-data",
                ]
            },
        },
        indent=2,
    )
    + "\n"
)
print("Prepared isolated network, synthetic credentials and bounded resources:", out)
