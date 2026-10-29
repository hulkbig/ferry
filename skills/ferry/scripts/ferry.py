#!/usr/bin/env python3
"""Dependency-free Ferry management and inference client."""

from __future__ import annotations

import argparse
import json
import mimetypes
import os
from pathlib import Path
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
import uuid


METHODS = ("GET", "POST", "PUT", "PATCH", "DELETE", "HEAD")
DATA_PATHS = {
    "/v1/chat/completions",
    "/v1/responses",
    "/v1/messages",
    "/v1/images/generations",
    "/v1/images/edits",
    "/v1/models",
    "/health",
    "/healthz",
    "/version",
}


def resolve_plane_config(plane: str, base_url: str | None = None, require_credential: bool = True) -> tuple[str, str]:
    """Choose one complete credential namespace; never combine legacy and new values."""
    url_key, token_key = (("MANAGEMENT_URL", "MANAGEMENT_TOKEN") if plane == "management"
                          else ("DATA_URL", "API_KEY"))
    def group(prefix):
        names = (prefix + url_key, prefix + token_key)
        present = any(name in os.environ for name in names)
        values = tuple(os.environ.get(name, "") for name in names)
        if present and (not (values[0] or base_url) or (require_credential and not values[1])
                        or any(name in os.environ and not os.environ[name] for name in names)):
            raise ValueError(prefix + url_key + " and " + prefix + token_key + " must form a complete configuration")
        return present, values
    new_present, new = group("FERRY_")
    old_present, old = group("SMARTAPI_")
    if new_present and old_present and new != old:
        raise ValueError("FERRY_ and SMARTAPI_ configurations conflict; select one namespace")
    url, token = new if new_present else old
    if require_credential and not token:
        raise ValueError("FERRY_" + token_key + " is required (legacy SMARTAPI_ pair is also supported)")
    if not (base_url or url):
        raise ValueError("FERRY_" + url_key + " or --base-url is required")
    return base_url or url, token


def url_origin(value: str) -> tuple[str, str, int | None]:
    parsed = urllib.parse.urlsplit(value)
    port = parsed.port
    if port is None:
        port = 443 if parsed.scheme.lower() == "https" else 80 if parsed.scheme.lower() == "http" else None
    return parsed.scheme.lower(), (parsed.hostname or "").lower(), port


class SameOriginRedirectHandler(urllib.request.HTTPRedirectHandler):
    """Allow normal redirects without ever forwarding credentials cross-origin."""

    def redirect_request(self, request, file_pointer, code, message, headers, new_url):
        if url_origin(request.full_url) != url_origin(new_url):
            raise ValueError("cross-origin redirect refused to protect Ferry credentials")
        return super().redirect_request(request, file_pointer, code, message, headers, new_url)


def management_high_risk(method: str, path: str) -> bool:
    clean_path = urllib.parse.urlsplit(path).path
    if clean_path == "/api/settings" and method not in ("GET", "HEAD"):
        return True
    if clean_path.startswith("/api/secrets"):
        return True
    if clean_path.startswith("/api/api-keys"):
        return not (method == "GET" and not clean_path.endswith("/secret"))
    if clean_path.endswith(("/routing/apply", "/apply", "/rotate-secret", "/clear-cooldown", "/set-quota", "/models/sync")):
        return True
    if ("/oauth/" in clean_path or clean_path.endswith("/oauth")) and method != "GET":
        return True
    return False


def checked_header(value: str) -> tuple[str, str]:
    if ":" not in value:
        raise argparse.ArgumentTypeError("header must use NAME: VALUE")
    name, content = value.split(":", 1)
    name, content = name.strip(), content.strip()
    if not re.fullmatch(r"[!#$%&'*+.^_`|~0-9A-Za-z-]+", name) or "\n" in content or "\r" in content:
        raise argparse.ArgumentTypeError("invalid header")
    if name.lower() in {
        "authorization", "proxy-authorization", "x-api-key", "cookie", "x-csrf-token", "content-type",
        "host", "content-length", "transfer-encoding", "connection",
    }:
        raise argparse.ArgumentTypeError(f"{name} is managed by the client and cannot be overridden")
    return name, content


def split_assignment(value: str, kind: str) -> tuple[str, str]:
    if "=" not in value:
        raise ValueError(f"{kind} must use NAME=VALUE")
    name, content = value.split("=", 1)
    if not re.fullmatch(r"[A-Za-z0-9_.-]+", name):
        raise ValueError(f"invalid {kind} field name {name!r}")
    return name, content


def json_body(args: argparse.Namespace) -> tuple[bytes | None, str | None]:
    if args.json is not None and args.json_file is not None:
        raise ValueError("use only one of --json and --json-file")
    if (args.json is not None or args.json_file is not None) and (args.form or args.file):
        raise ValueError("JSON and multipart inputs cannot be combined")
    if args.json is None and args.json_file is None:
        return None, None
    if args.json is not None:
        raw = args.json.encode("utf-8")
    elif args.json_file == "-":
        raw = sys.stdin.buffer.read()
    else:
        raw = Path(args.json_file).read_bytes()
    json.loads(raw)
    return raw, "application/json"


def multipart_body(args: argparse.Namespace) -> tuple[bytes | None, str | None]:
    if not args.form and not args.file:
        return None, None
    boundary = "ferry-" + uuid.uuid4().hex
    chunks: list[bytes] = []

    def add_line(value: str) -> None:
        chunks.append(value.encode("utf-8") + b"\r\n")

    for raw in args.form:
        name, value = split_assignment(raw, "form")
        add_line("--" + boundary)
        add_line(f'Content-Disposition: form-data; name="{name}"')
        add_line("")
        add_line(value)
    for raw in args.file:
        name, filename = split_assignment(raw, "file")
        path = Path(filename)
        if not path.is_file():
            raise ValueError(f"file not found: {path}")
        media_type = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
        safe_name = path.name.replace('"', "_")
        add_line("--" + boundary)
        add_line(f'Content-Disposition: form-data; name="{name}"; filename="{safe_name}"')
        add_line(f"Content-Type: {media_type}")
        add_line("")
        chunks.append(path.read_bytes())
        chunks.append(b"\r\n")
    add_line("--" + boundary + "--")
    return b"".join(chunks), f"multipart/form-data; boundary={boundary}"


def request_body(args: argparse.Namespace) -> tuple[bytes | None, str | None]:
    body, content_type = json_body(args)
    if body is not None:
        return body, content_type
    return multipart_body(args)


def validated_url(base_url: str, path: str) -> str:
    parsed = urllib.parse.urlsplit(base_url)
    if parsed.scheme not in {"http", "https"} or not parsed.netloc or parsed.username or parsed.password:
        raise ValueError("base URL must be an absolute HTTP(S) URL without embedded credentials")
    if not path.startswith("/") or path.startswith("//"):
        raise ValueError("path must start with one slash")
    return base_url.rstrip("/") + path


MANAGEMENT_RESOURCES = {
    "oauth",
    "access",
    "security-events",
    "secrets",
    "overview",
    "analytics",
    "organizations",
    "usage-billing",
    "alerts",
    "models",
    "providers",
    "requests",
    "queue",
    "settings",
    "api-keys",
    "drafts",
    "revisions",
}


def is_management_api_path(path: str) -> bool:
    if not path.startswith("/api/"):
        return False
    resource = path[len("/api/"):].split("/", 1)[0]
    return resource in MANAGEMENT_RESOURCES


def print_response(data: bytes, content_type: str, stream: bool) -> None:
    if stream:
        return
    if not data:
        return
    if "json" in content_type.lower():
        try:
            value = json.loads(data)
            print(json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True))
            return
        except json.JSONDecodeError:
            pass
    sys.stdout.buffer.write(data)
    if not data.endswith(b"\n"):
        sys.stdout.buffer.write(b"\n")


def execute(args: argparse.Namespace) -> int:
    method = args.method.upper()
    clean_path = urllib.parse.urlsplit(args.path).path
    if args.plane == "management":
        if clean_path.startswith("/api/management-tokens"):
            raise ValueError("Management Token identities are administrator-session-only; manage them in the Ferry console")
        if not is_management_api_path(clean_path):
            raise ValueError("management requests must use a documented /api/* product path; legacy, versioned, and /api/auth/* paths are unavailable")
        if management_high_risk(method, args.path) and not args.confirm_high_risk:
            raise ValueError("this is a high-risk operation; confirm user intent and rerun with --confirm-high-risk (the Ferry Console token switch must also be enabled)")
    else:
        if clean_path not in DATA_PATHS:
            raise ValueError(f"unsupported data-plane path: {clean_path}")
    base_url, credential = resolve_plane_config(args.plane, args.base_url,
        require_credential=args.plane == "management" or clean_path.startswith("/v1/"))

    body, content_type = request_body(args)
    headers = {"Accept": "text/event-stream, application/json" if args.stream else "application/json", "User-Agent": "ferry-skill/1"}
    if credential:
        headers["Authorization"] = "Bearer " + credential
    if content_type:
        headers["Content-Type"] = content_type
    headers.update(dict(args.header))
    request = urllib.request.Request(validated_url(base_url, args.path), data=body, headers=headers, method=method)
    try:
        opener = urllib.request.build_opener(SameOriginRedirectHandler())
        with opener.open(request, timeout=args.timeout) as response:
            if args.stream:
                while True:
                    chunk = response.read(8192)
                    if not chunk:
                        break
                    sys.stdout.buffer.write(chunk)
                    sys.stdout.buffer.flush()
                return 0
            data = response.read()
            print_response(data, response.headers.get("Content-Type", ""), False)
            return 0
    except urllib.error.HTTPError as error:
        data = error.read()
        print_response(data, error.headers.get("Content-Type", ""), False)
        if error.code == 403 and args.plane == "management":
            try:
                code = json.loads(data).get("error", {}).get("code")
            except (json.JSONDecodeError, AttributeError):
                code = ""
            if code == "HIGH_RISK_NOT_ALLOWED":
                print("High-risk access is blocked for this token. Ask the administrator to enable its switch in Ferry Console > Settings > Security.", file=sys.stderr)
        return 1
    except urllib.error.URLError as error:
        print(f"request failed: {error.reason}", file=sys.stderr)
        return 1


def parser() -> argparse.ArgumentParser:
    root = argparse.ArgumentParser(description="Call Ferry without third-party dependencies or credential arguments.")
    subparsers = root.add_subparsers(dest="plane", required=True)
    for plane in ("management", "inference"):
        command = subparsers.add_parser(plane)
        command.add_argument("method", choices=METHODS, type=str.upper)
        command.add_argument("path", help="absolute API path, including any query string")
        command.add_argument("--base-url", help="non-secret override for the configured plane URL")
        command.add_argument("--json", help="inline JSON request body")
        command.add_argument("--json-file", help="JSON request file, or - for stdin")
        command.add_argument("--form", action="append", default=[], metavar="NAME=VALUE", help="multipart text field; repeatable")
        command.add_argument("--file", action="append", default=[], metavar="NAME=PATH", help="multipart file field; repeatable")
        command.add_argument("--header", action="append", default=[], type=checked_header, metavar="NAME:VALUE", help="non-authentication request header; repeatable")
        command.add_argument("--stream", action="store_true", help="copy the response body incrementally (for SSE)")
        command.add_argument("--timeout", type=float, default=30, help="request timeout in seconds; default 30")
        command.add_argument("--confirm-high-risk", action="store_true", help="confirm explicit user intent for a high-risk management request")
    return root


def main(argv: list[str] | None = None) -> int:
    args = parser().parse_args(argv)
    try:
        return execute(args)
    except (ValueError, OSError, json.JSONDecodeError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
