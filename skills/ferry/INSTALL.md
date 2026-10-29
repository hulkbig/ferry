# Install Ferry from this instance

No marketplace, GitHub access, npm, or pip dependency is required. Python 3 is
needed to run the bundled client. This directory is shipped inside the Ferry
binary; the documentation, scripts and ZIP all come from the same release.

## Paste into your assistant

Replace `<Ferry URL>` with the instance URL visible in the console:

> Install the Ferry Skill from <Ferry URL>/skills/ferry/SKILL.md.
> First read the adjacent INSTALL.md. Download ferry.zip and
> ferry.zip.sha256 from that same directory, verify SHA-256, inspect the
> archive, and install its ferry folder in this assistant's supported user
> skill directory. Preserve any existing installation before replacing it.
> Read the installed SKILL.md. Configure access through an existing trusted
> secret injector; do not ask me to paste secrets into this chat. Call
> GET /api/access to verify management access. Report the installed path,
> instance URL and readiness result. If credentials or local filesystem access
> are unavailable, state what remains; do not claim ready.

The console's **Settings → Security → Install Ferry Skill** provides this
prompt with the current origin filled in. Installation does not create a token,
change routing, grant high-risk access, or perform a paid inference request.

## Files

- [SKILL.md](SKILL.md): entry point and operating rules.
- [ferry.zip](ferry.zip): complete folder, including scripts and references.
- [ferry.zip.sha256](ferry.zip.sha256): checksum of this release's ZIP.

Download both files over your trusted instance's HTTPS connection. A checksum
checks transfer consistency; it does not authenticate an untrusted server.
Extract only the `ferry/` tree; reject absolute paths, `..`, and symlinks.
Do not install only SKILL.md: its relative scripts and references are required.

For assistants with filesystem access, select the skill directory supported by
that host. If a ferry directory already exists, compare it first and preserve
local changes. For a web-only assistant, reading this URL provides instructions
but is not a persistent installation; use that host's supported file import or
connection mechanism. A host may require reloading its skills after installation.

## Access and readiness

Supply `FERRY_MANAGEMENT_URL` and `FERRY_MANAGEMENT_TOKEN` through your
secret manager. Create a dedicated Management Token in **Settings → Security**
if necessary; never include it in an installation URL or copied prompt. The
management URL is the instance origin, without `/api`. Data-plane access is
optional and uses a separate API key. Each installation needs its own credentials; no particular secret manager is required.

Run the installed client through the injector:

```text
python3 <installed-skill>/scripts/ferry.py management GET /api/access
```

Only a successful access response establishes management readiness. Report its
safe token identity and high-risk status, never the raw token. Missing credentials,
401, 403, or network failures mean installed but not ready; do not fall back to
another user's token. Leave high-risk operations disabled unless explicitly needed.

Legacy `SMARTAPI_` variables are accepted as complete URL/credential pairs when the corresponding `FERRY_` pair is absent. Do not mix namespaces; conflicting or partial groups fail before a request is sent. Preserve an existing SmartAPI Skill if it still manages a separate instance.
