#!/usr/bin/env python3
"""Preview/apply OpenCodeX Fast declarations from Ferry's authenticated catalog.

Only GET /models is sent. Credentials come from FERRY_API_KEY, never arguments.
Configuration writes use the supported ocx CLI; no installed package is patched.
This standalone helper runs from the installed Ferry Skill directory.
"""

from ferry import resolve_plane_config, url_origin

import argparse
import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request


MAX_CATALOG_BYTES = 4 * 1024 * 1024
OPENAI_ADAPTERS = {"openai-chat", "openai-responses"}


class SyncError(Exception):
    pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise SyncError("Model catalog redirect refused; verify the configured baseUrl")


def ocx_command(binary, *args):
    # Do not relay CLI output on failure: it may contain unrelated private config.
    try:
        result = subprocess.run(
            [binary, *args], capture_output=True, text=True, timeout=120, check=False
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise SyncError("OpenCodeX command unavailable or timed out") from exc
    if result.returncode:
        raise SyncError("OpenCodeX command failed (exit %d)" % result.returncode)
    return result.stdout


def read_provider(binary, name):
    try:
        provider = json.loads(ocx_command(binary, "config", "get", "providers." + name, "--json"))
    except (ValueError, TypeError) as exc:
        raise SyncError("OpenCodeX did not return a provider JSON object") from exc
    if not isinstance(provider, dict):
        raise SyncError("OpenCodeX provider does not exist")
    return provider


def catalog_url(provider):
    if provider.get("adapter") not in OPENAI_ADAPTERS:
        raise SyncError("The Ferry provider must use openai-chat or openai-responses")
    base = provider.get("baseUrl")
    if not isinstance(base, str):
        raise SyncError("Provider baseUrl is missing")
    try:
        url = urllib.parse.urlsplit(base)
        secure = url.scheme == "https" or (
            url.scheme == "http" and url.hostname in {"localhost", "127.0.0.1", "::1"}
        )
        if not secure or not url.hostname or url.username or url.password or url.query or url.fragment:
            raise ValueError("invalid base URL")
        # Accessing port also validates malformed authority syntax.
        _ = url.port
    except ValueError as exc:
        raise SyncError("baseUrl must be HTTPS (or local HTTP), without credentials, query or fragment") from exc
    return base.rstrip("/") + "/models"


def fetch_models(url, api_key):
    if not api_key or any(char in api_key for char in "\r\n"):
        raise SyncError("FERRY_API_KEY must contain the consumer key used with this gateway")
    request = urllib.request.Request(url, headers={"Authorization": "Bearer " + api_key, "Accept": "application/json"})
    try:
        with urllib.request.build_opener(NoRedirect()).open(request, timeout=20) as response:
            body = response.read(MAX_CATALOG_BYTES + 1)
    except urllib.error.HTTPError as exc:
        raise SyncError("Model catalog returned HTTP %d; no config changed" % exc.code) from exc
    except (OSError, urllib.error.URLError) as exc:
        raise SyncError("Model catalog could not be read; no config changed") from exc
    if len(body) > MAX_CATALOG_BYTES:
        raise SyncError("Model catalog exceeds the size limit")
    try:
        return json.loads(body)
    except (ValueError, UnicodeError) as exc:
        raise SyncError("Model catalog is not valid JSON") from exc


def capability_plan(provider, catalog):
    catalog_url(provider)
    # A provider-wide true would enable future models before they are declared.
    if provider.get("supportsServiceTier") is True:
        raise SyncError("Remove provider-wide supportsServiceTier=true before syncing exact model declarations")
    if "fastWire" in provider:
        wire = provider["fastWire"]
        mapping = wire.get("canonicalToWire") if isinstance(wire, dict) else None
        if not isinstance(mapping, dict) or wire.get("kind") != "service-tier" or mapping.get("priority") != "priority":
            raise SyncError("fastWire must preserve service_tier=priority for Ferry")
    previous = provider.get("modelSupportsServiceTier", {})
    adapters = provider.get("modelAdapters", {})
    if not isinstance(previous, dict) or any(type(value) is not bool for value in previous.values()):
        raise SyncError("Existing modelSupportsServiceTier is invalid")
    if not isinstance(adapters, dict):
        raise SyncError("Existing modelAdapters is invalid")
    rows = catalog.get("data") if isinstance(catalog, dict) else None
    if not isinstance(rows, list) or not rows:
        raise SyncError("Model catalog is empty or invalid; no config changed")
    # Keep explicit false entries for removed models, overriding old true values.
    plan = {model: False for model in previous}
    seen = set()
    for row in rows:
        if not isinstance(row, dict) or not isinstance(row.get("id"), str) or not row["id"].strip():
            raise SyncError("Model catalog contains an invalid model ID")
        model = row["id"]
        if model in seen:
            raise SyncError("Model catalog contains duplicate model IDs")
        seen.add(model)
        tiers = row.get("supported_service_tiers")
        # Missing metadata is an old/unknown server, never an unsupported verdict.
        if not isinstance(tiers, dict) or any(
            not isinstance(tiers.get(protocol), list)
            or any(not isinstance(tier, str) for tier in tiers[protocol])
            for protocol in OPENAI_ADAPTERS
        ):
            raise SyncError("Ferry lacks supported_service_tiers metadata; deploy the server fix first")
        adapter = adapters.get(model, provider["adapter"])
        plan[model] = adapter in OPENAI_ADAPTERS and "priority" in tiers[adapter]
    return dict(sorted(plan.items()))


def tier_authority_plan(provider, catalog):
    # OpenCodeX currently exposes a provider-wide switch. Only an entirely
    # authoritative catalog can safely opt in; mixed or unknown routes opt out.
    values = []
    adapters = provider.get("modelAdapters", {})
    for row in catalog["data"]:
        adapter = adapters.get(row["id"], provider["adapter"])
        if adapter not in OPENAI_ADAPTERS:
            continue
        authority = row.get("response_tier_authority")
        if not isinstance(authority, dict) or any(
            not isinstance(authority.get(protocol), str)
            or authority[protocol] not in {"authoritative", "informational", "unknown"}
            for protocol in OPENAI_ADAPTERS
        ):
            raise SyncError("Ferry lacks response_tier_authority metadata; deploy the server fix first")
        values.append(authority[adapter])
    return bool(values) and all(value == "authoritative" for value in values)


def synchronize(binary, name, apply=False, sync_tier_authority=False):
    if not re.fullmatch(r"[A-Za-z0-9_-]+", name):
        raise SyncError("Provider name must contain only letters, digits, underscores or hyphens")
    provider = read_provider(binary, name)
    url = catalog_url(provider)
    try:
        _, token = resolve_plane_config("data", provider["baseUrl"])
        for variable in ("FERRY_DATA_URL", "SMARTAPI_DATA_URL"):
            if os.environ.get(variable) and url_origin(os.environ[variable]) != url_origin(url):
                raise ValueError("Configured data URL differs from OpenCodeX provider origin")
    except ValueError as exc:
        raise SyncError(str(exc)) from exc
    catalog = fetch_models(url, token)
    plan = capability_plan(provider, catalog)
    changed = plan != provider.get("modelSupportsServiceTier", {})
    authority = tier_authority_plan(provider, catalog) if sync_tier_authority else None
    authority_changed = sync_tier_authority and provider.get("responseTierAuthoritative") is not authority
    if apply:
        if read_provider(binary, name) != provider:
            raise SyncError("Provider changed during catalog read; rerun to review the new configuration")
        if authority_changed:
            try:
                ocx_command(binary, "config", "set", "providers." + name + ".responseTierAuthoritative", json.dumps(authority), "--json")
            except SyncError as exc:
                raise SyncError("Tier authority update did not complete; read back config before retrying (requires OpenCodeX responseTierAuthoritative support)") from exc
            expected = dict(provider, responseTierAuthoritative=authority)
            if read_provider(binary, name) != expected:
                raise SyncError("Tier authority was not saved as expected or provider changed; verify OpenCodeX supports responseTierAuthoritative before retrying")
        if changed:
            try:
                ocx_command(binary, "config", "set", "providers." + name + ".modelSupportsServiceTier", json.dumps(plan), "--json")
            except SyncError as exc:
                raise SyncError("Config update did not complete; read back OpenCodeX config before retrying") from exc
        try:
            ocx_command(binary, "sync")
        except SyncError as exc:
            raise SyncError("Model declarations are saved, but ocx sync failed; rerun this command to finish") from exc
    result = {"provider": name, "applied": apply, "changed": changed or authority_changed, "modelSupportsServiceTier": plan}
    if sync_tier_authority:
        result["responseTierAuthoritative"] = authority
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--provider", required=True, help="Existing OpenCodeX provider ID (never renamed)")
    parser.add_argument("--ocx", default="ocx", help="OpenCodeX CLI executable")
    parser.add_argument("--apply", action="store_true", help="Save exact declarations and run ocx sync (default: preview)")
    parser.add_argument("--sync-tier-authority", action="store_true", help="Also sync responseTierAuthoritative (requires an OpenCodeX version supporting this field)")
    args = parser.parse_args(argv)
    try:
        result = synchronize(args.ocx, args.provider, args.apply, args.sync_tier_authority)
    except SyncError as exc:
        print("error: " + str(exc), file=sys.stderr)
        return 1
    print(json.dumps(result, indent=2, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
