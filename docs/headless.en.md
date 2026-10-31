# Headless operation

Available in 1.1.0; while this is a release candidate, do not assume the image is already published. Version 1.0.0 does not support these flags.

A normal Ferry binary can run with `-headless`. It opens no management HTTP listener and starts no optional alert polling, version checks, model/catalog sync, scheduled messages, automatic quota resets or reference-pricing sync. Authentication, routing, quotas, request ledger, conversation continuity, cooldown persistence and account-token refresh remain active. `-standby` still disables account refresh.

```sh
ferry -headless -data-dir ./data
```

Configure accounts/routes/client keys first. Headless has no online management UI; stop it before opening the same data directory in regular mode. Never run two instances against one SQLite data directory. An empty instance is not automatically ready to call upstreams.

## Docker without building

The ordinary 1.1.0 prebuilt image supports runtime headless mode. Initialize using the [regular Docker setup](guide/docker.en.md), stop that instance, then use the same named data volume:

```sh
docker compose down
curl -fsSLo compose.headless.yaml https://raw.githubusercontent.com/hulkbig/ferry/v1.1.0/deploy/compose.headless.yaml
docker compose -f compose.headless.yaml up -d --no-build
```

Do not use `down -v`. This file uses `ferry-data` and `/data/state`, matching new installations. The legacy deploy Compose uses `/data`; verify that layout before switching. Custom volumes use `FERRY_DATA_VOLUME`. Only inference is published. Drain active requests before changing modes; stop headless before returning to the original Compose.

## Dedicated developer build

```sh
go build -tags headless -o ferry-headless ./cmd/ferry
```

This build excludes console assets and cannot be overridden with `-headless=false`. No separate prebuilt headless-image tag is claimed. Runtime headless in the normal Docker image does not remove embedded files from that image.

`-startup-check -headless` returns an empty management address. Inference and its health endpoint retain their configured addresses. [Performance evidence and limits](guide/performance.en.md).
