"""Regressions for the standalone OpenCodeX Fast declaration sync."""

import copy
import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

import sync_opencodex_fast as sync


def provider():
    return {"adapter": "openai-responses", "baseUrl": "https://gateway.example/v1", "modelSupportsServiceTier": {"removed": True}}


def catalog():
    return {"data": [
        {"id": "gpt-6-astra", "supported_service_tiers": {"openai-responses": ["priority"], "openai-chat": []}},
        {"id": "gpt-5.6-sol", "supported_service_tiers": {"openai-responses": [], "openai-chat": ["priority"]}},
    ]}


class CapabilityPlanTests(unittest.TestCase):
    def test_exact_evidence_and_revocation(self):
        self.assertEqual(sync.capability_plan(provider(), catalog()), {
            "gpt-6-astra": True, "gpt-5.6-sol": False, "removed": False,
        })

    def test_final_model_adapter_controls_tier(self):
        config = provider()
        config["modelAdapters"] = {"gpt-6-astra": "openai-chat", "gpt-5.6-sol": "openai-chat"}
        plan = sync.capability_plan(config, catalog())
        self.assertFalse(plan["gpt-6-astra"])
        self.assertTrue(plan["gpt-5.6-sol"])

    def test_old_or_invalid_server_cannot_overwrite_config(self):
        for payload in ({}, {"data": []}, {"data": [{"id": "gpt-6-astra"}]}, {"data": catalog()["data"] * 2}):
            with self.subTest(payload=payload), self.assertRaises(sync.SyncError):
                sync.capability_plan(provider(), payload)

    def test_wire_and_provider_defaults_cannot_bypass_evidence(self):
        for field, value in (
            ("supportsServiceTier", True), ("fastWire", None),
            ("fastWire", {"kind": "service-tier", "canonicalToWire": {"priority": "fast"}}),
            ("fastWire", {"kind": "service-tier", "canonicalToWire": None}),
        ):
            config = provider()
            config[field] = value
            with self.subTest(field=field, value=value), self.assertRaises(sync.SyncError):
                sync.capability_plan(config, catalog())

    def test_credentials_are_not_sent_to_unsafe_urls(self):
        for base in ("http://gateway.example/v1", "https://user:secret@gateway.example/v1", "https://gateway.example/v1?key=secret"):
            config = provider()
            config["baseUrl"] = base
            with self.subTest(base=base), self.assertRaises(sync.SyncError):
                sync.catalog_url(config)
        self.assertEqual(sync.catalog_url(provider()), "https://gateway.example/v1/models")

    def test_redirects_are_refused(self):
        with self.assertRaises(sync.SyncError):
            sync.NoRedirect().redirect_request(None, None, 302, "", {}, "https://other.example")


@patch.dict("os.environ", {"FERRY_API_KEY": "fixture-key"}, clear=True)
class SynchronizeTests(unittest.TestCase):
    def test_preview_only_reads(self):
        with patch.object(sync, "read_provider", return_value=provider()), patch.object(sync, "fetch_models", return_value=catalog()), patch.object(sync, "ocx_command") as command:
            result = sync.synchronize("ocx", "ferry")
        command.assert_not_called()
        self.assertFalse(result["applied"])

    def test_apply_sets_entire_map_and_syncs_without_key_arguments(self):
        with patch.object(sync, "read_provider", return_value=provider()), patch.object(sync, "fetch_models", return_value=catalog()), patch.object(sync, "ocx_command") as command:
            result = sync.synchronize("ocx", "ferry", apply=True)
        args = command.call_args_list[0].args
        self.assertEqual(args[:4], ("ocx", "config", "set", "providers.ferry.modelSupportsServiceTier"))
        self.assertEqual(json.loads(args[4]), result["modelSupportsServiceTier"])
        self.assertEqual(command.call_args_list[1].args, ("ocx", "sync"))

    def test_apply_checks_concurrent_provider_changes(self):
        changed = copy.deepcopy(provider())
        changed["baseUrl"] = "https://other.example/v1"
        with patch.object(sync, "read_provider", side_effect=[provider(), changed]), patch.object(sync, "fetch_models", return_value=catalog()), patch.object(sync, "ocx_command") as command:
            with self.assertRaisesRegex(sync.SyncError, "changed during"):
                sync.synchronize("ocx", "ferry", apply=True)
        command.assert_not_called()

    def test_partial_failure_can_resume_without_rewriting(self):
        saved = provider()
        saved["modelSupportsServiceTier"] = sync.capability_plan(saved, catalog())
        with patch.object(sync, "read_provider", return_value=saved), patch.object(sync, "fetch_models", return_value=catalog()), patch.object(sync, "ocx_command", side_effect=sync.SyncError("failed")) as command:
            with self.assertRaisesRegex(sync.SyncError, "declarations are saved, but ocx sync failed"):
                sync.synchronize("ocx", "ferry", apply=True)
        command.assert_called_once_with("ocx", "sync")


class CatalogHTTPTests(unittest.TestCase):
    def test_metadata_get_and_redirect_refusal(self):
        requests = []

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                requests.append((self.command, self.path, self.headers.get("Authorization")))
                if self.path == "/redirect":
                    self.send_response(302)
                    self.send_header("Location", "/must-not-receive-key")
                    self.end_headers()
                    return
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps(catalog()).encode())

            def log_message(self, *_args):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        try:
            base = "http://127.0.0.1:%d" % server.server_port
            self.assertEqual(sync.fetch_models(base + "/v1/models", "test-key"), catalog())
            with self.assertRaises(sync.SyncError):
                sync.fetch_models(base + "/redirect", "test-key")
            self.assertEqual(requests, [
                ("GET", "/v1/models", "Bearer test-key"),
                ("GET", "/redirect", "Bearer test-key"),
            ])
        finally:
            server.shutdown()
            server.server_close()
            worker.join()


@patch.dict("os.environ", {"FERRY_API_KEY": "fixture-key"}, clear=True)
class TierAuthorityTests(unittest.TestCase):
    def catalog(self, authority="informational"):
        data = catalog()
        for row in data["data"]:
            row["response_tier_authority"] = {protocol: authority for protocol in sync.OPENAI_ADAPTERS}
        return data

    def test_provider_switch_is_conservative_and_separate_from_capability(self):
        for value in ("authoritative", "informational", "unknown"):
            data = self.catalog(value)
            self.assertEqual(sync.tier_authority_plan(provider(), data), value == "authoritative")
            self.assertEqual(sync.capability_plan(provider(), data), sync.capability_plan(provider(), catalog()))
        data = self.catalog("authoritative")
        data["data"][1]["response_tier_authority"]["openai-responses"] = "informational"
        self.assertFalse(sync.tier_authority_plan(provider(), data))
        config = provider()
        config["modelAdapters"] = {"gpt-5.6-sol": "openai-chat"}
        self.assertTrue(sync.tier_authority_plan(config, data))

    def test_opt_in_preview_does_not_write(self):
        with patch.object(sync, "read_provider", return_value=provider()), patch.object(sync, "fetch_models", return_value=self.catalog()), patch.object(sync, "ocx_command") as command:
            result = sync.synchronize("ocx", "ferry", sync_tier_authority=True)
        self.assertIs(result["responseTierAuthoritative"], False)
        command.assert_not_called()

    def test_missing_metadata_fails_before_writing(self):
        with patch.object(sync, "read_provider", return_value=provider()), patch.object(sync, "fetch_models", return_value=catalog()), patch.object(sync, "ocx_command") as command:
            with self.assertRaisesRegex(sync.SyncError, "lacks response_tier_authority"):
                sync.synchronize("ocx", "ferry", apply=True, sync_tier_authority=True)
        command.assert_not_called()

    def test_apply_verifies_authority_before_capabilities(self):
        saved = dict(provider(), responseTierAuthoritative=False)
        with patch.object(sync, "read_provider", side_effect=[provider(), provider(), saved]), patch.object(sync, "fetch_models", return_value=self.catalog()), patch.object(sync, "ocx_command") as command:
            result = sync.synchronize("ocx", "ferry", apply=True, sync_tier_authority=True)
        self.assertIs(result["responseTierAuthoritative"], False)
        self.assertEqual(command.call_args_list[0].args, ("ocx", "config", "set", "providers.ferry.responseTierAuthoritative", "false", "--json"))
        self.assertEqual(command.call_args_list[1].args[3], "providers.ferry.modelSupportsServiceTier")
        self.assertEqual(command.call_args_list[2].args, ("ocx", "sync"))

    def test_old_client_or_concurrent_change_cannot_silently_ignore_authority(self):
        changed = dict(provider(), responseTierAuthoritative=False, baseUrl="https://other.example/v1")
        for readback in (provider(), changed):
            with patch.object(sync, "read_provider", side_effect=[provider(), provider(), readback]), patch.object(sync, "fetch_models", return_value=self.catalog()), patch.object(sync, "ocx_command") as command:
                with self.assertRaisesRegex(sync.SyncError, "not saved as expected"):
                    sync.synchronize("ocx", "ferry", apply=True, sync_tier_authority=True)
            self.assertEqual(command.call_count, 1)

    def test_resuming_saved_authority_only_syncs(self):
        saved = dict(provider(), responseTierAuthoritative=False)
        saved["modelSupportsServiceTier"] = sync.capability_plan(saved, self.catalog())
        with patch.object(sync, "read_provider", return_value=saved), patch.object(sync, "fetch_models", return_value=self.catalog()), patch.object(sync, "ocx_command") as command:
            result = sync.synchronize("ocx", "ferry", apply=True, sync_tier_authority=True)
        self.assertFalse(result["changed"])
        command.assert_called_once_with("ocx", "sync")


if __name__ == "__main__":
    unittest.main()
