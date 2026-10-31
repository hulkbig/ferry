# Install with Docker

[中文](docker.md)


## Start Ferry

Install Docker Engine with Compose. An existing OrbStack or Docker Desktop installation on macOS is sufficient. Linux AMD64 and ARM64 images are selected automatically.

In an empty directory:

```sh
curl -fsSLo compose.yaml https://github.com/hulkbig/ferry/releases/latest/download/compose.yaml
docker compose up -d --no-build
```

Open **`http://localhost:17910`**, then create your administrator. No source checkout, compilation, `.env` file or separate database is needed.

1. Add your upstream and credentials under **Providers**.
2. Check the logical model and its enabled connections under **Models & Routing**.
3. Create a client key under **API Keys**. Set your client's base URL to `http://localhost:17810/v1` and select your logical model.

You supply the upstream account and credits. A minimal first request:

```sh
curl http://localhost:17810/v1/chat/completions \
  -H "Authorization: Bearer $FERRY_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"your-logical-model","messages":[{"role":"user","content":"Say hello briefly."}]}'
```

Set `FERRY_API_KEY` through your local secret manager or a private shell session. Do not publish credentials in issue reports.

### Remote server

Ports bind to loopback by default. From your computer, open a tunnel:

```sh
ssh -N -L 17910:127.0.0.1:17910 -L 17810:127.0.0.1:17810 user@your-server
```

Visit `http://localhost:17910`. For shared access, initialize the administrator first, then configure an HTTPS reverse proxy with access restrictions. Proxy inference to `17810` and management to `17910`.

### Without Compose

```sh
docker run -d --name ferry --restart unless-stopped \
  -p 127.0.0.1:17910:7900 -p 127.0.0.1:17810:7890 \
  -v ferry-data:/data --read-only --tmpfs /tmp:size=32m,mode=1777 \
  --cap-drop ALL --security-opt no-new-privileges:true \
  bighulk/ferry:1.1.0
```

Choose one installation method; never run two processes against the same data.

## Data and maintenance

The `ferry-data` volume mounts at `/data`; databases live in `/data/state`. Credentials, settings and request history survive container replacement. Ferry runs as UID/GID `10001:10001`. Use local storage and one instance per database.

```sh
docker compose ps
docker compose logs --tail=100 ferry
docker compose stop
docker compose up -d --no-build
```

Do not add `--volumes` to `docker compose down` unless you intend to delete the data. A healthy `/health` response does not verify an upstream model.

## Back up and restore

Use a new backup directory each time:

```sh
docker compose exec -T ferry /usr/local/bin/ferry -data-dir /data/state -backup-to /data/backups/before-upgrade
docker compose exec -T ferry /usr/local/bin/ferry -verify-backup /data/backups/before-upgrade
umask 077
docker compose cp ferry:/data/backups/before-upgrade ./backup-before-upgrade
```

The default backup contains only current configuration, the administrator, API keys, OAuth/upstream credentials and runtime settings. It excludes request history, conversations, old revisions, drafts and login sessions. Backups still contain sensitive credentials; keep a protected copy on independent storage. Do not copy live database files directly.

To retain full history across an upgrade or migration, use a new directory and explicitly add `-backup-history`:

```sh
docker compose exec -T ferry /usr/local/bin/ferry -data-dir /data/state -backup-to /data/backups/full-migration -backup-history
docker compose exec -T ferry /usr/local/bin/ferry -verify-backup /data/backups/full-migration
```

Copy full backups to independent storage as well. Scheduled backups are always configuration-only and cannot replace a full-history backup.

Restore the verified backup retained in the volume:

```sh
docker compose stop ferry
docker compose run --rm --no-deps ferry -data-dir /data/state -restore-from /data/backups/before-upgrade
docker compose up -d --no-build
```

Restoring replaces current data with the selected backup; later writes are not merged. Restoring a configuration backup leaves history empty; only an explicit full backup restores history from the backup point. If restoration fails, investigate before restarting. Verify login, configuration and a real request afterward. Keep the restore journal and displaced data until verification is complete.

## Upgrade

Verify a backup and rehearse its restoration; use `-backup-history` if history must be retained. Read the target release notes. Change the image version in `compose.yaml`, then run:

```sh
docker compose pull
docker compose up -d --no-build
```

Stable version tags are not overwritten; `latest` follows stable releases. Pin a version for predictable upgrades. Reverting an image does not reverse a database migration.

There is no earlier public version for the first release's upgrade rehearsal. Older internal installations may store databases directly under `/data`; migrate through a verified backup instead of silently switching to `/data/state`.

## Troubleshooting

- **Port in use:** change the host port on the left of the Compose mapping.
- **Login does not persist:** use `localhost` locally; use HTTPS or an SSH tunnel remotely, rather than plain HTTP on a server IP.
- **Cannot pull:** verify Docker Hub connectivity and that the requested version has been published. No private infrastructure or OpenBao is required.
- **Model fails:** check key scope, upstream credits, exact model ID and connection status. Listing a model is not proof of working inference.

Report the version, architecture and a redacted reproduction in [GitHub Issues](https://github.com/hulkbig/ferry/issues).

[More questions](faq.en.md) · [Troubleshoot a request](troubleshooting.en.md) · [Contributing and PRs](../../CONTRIBUTING.en.md)
