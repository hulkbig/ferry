#!/usr/bin/env python3

from __future__ import annotations

from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest


CLIENT = Path(__file__).with_name("ferry.py")


class Handler(BaseHTTPRequestHandler):
    requests: list[dict[str, object]] = []
    redirect_target = ""

    def log_message(self, _format: str, *_args: object) -> None:
        pass

    def record(self) -> bytes:
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length) if length else b""
        self.requests.append({
            "method": self.command,
            "path": self.path,
            "authorization": self.headers.get("Authorization", ""),
            "content_type": self.headers.get("Content-Type", ""),
            "body": body,
        })
        return body

    def respond(self, status: int, body: dict[str, object]) -> None:
        raw = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self) -> None:
        self.record()
        if self.path == "/api/models/redirect":
            self.send_response(302)
            self.send_header("Location", self.redirect_target)
            self.end_headers()
            return
        self.respond(200, {"authenticated": True, "auth_type": "management_token"})

    def do_PATCH(self) -> None:
        self.record()
        self.respond(403, {"error": {"code": "HIGH_RISK_NOT_ALLOWED", "message": "blocked"}})

    def do_POST(self) -> None:
        self.record()
        self.respond(200, {"ok": True})


class FerryClientTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        Handler.requests = []
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()
        cls.url = f"http://127.0.0.1:{cls.server.server_port}"
        Handler.redirect_target = f"http://localhost:{cls.server.server_port}/api/models/capture"

    @classmethod
    def tearDownClass(cls) -> None:
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join(timeout=2)

    def run_client(self, *args: str) -> subprocess.CompletedProcess[str]:
        env = {
            **os.environ,
            "FERRY_MANAGEMENT_URL": self.url,
            "FERRY_MANAGEMENT_TOKEN": "smt_test_secret",
            "FERRY_DATA_URL": self.url,
            "FERRY_API_KEY": "sk-ferry-test",
        }
        return subprocess.run([sys.executable, str(CLIENT), *args], env=env, text=True,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)

    def test_management_token_is_sent_without_being_printed(self) -> None:
        before = len(Handler.requests)
        result = self.run_client("management", "GET", "/api/access")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(Handler.requests), before + 1)
        self.assertEqual(Handler.requests[-1]["authorization"], "Bearer smt_test_secret")
        self.assertNotIn("smt_test_secret", result.stdout + result.stderr)

    def test_queue_read_can_verify_request_drain(self) -> None:
        result = self.run_client("management", "GET", "/api/queue")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(Handler.requests[-1]["path"], "/api/queue")
        self.assertEqual(Handler.requests[-1]["authorization"], "Bearer smt_test_secret")
        self.assertNotIn("smt_test_secret", result.stdout + result.stderr)

    def test_high_risk_requires_local_confirmation_and_server_gate(self) -> None:
        before = len(Handler.requests)
        result = self.run_client("management", "PATCH", "/api/settings", "--json", '{"debug_request_response":true}')
        self.assertEqual(result.returncode, 2)
        self.assertEqual(len(Handler.requests), before)
        self.assertIn("--confirm-high-risk", result.stderr)

        result = self.run_client("management", "PATCH", "/api/settings", "--json", '{"debug_request_response":true}', "--confirm-high-risk")
        self.assertEqual(result.returncode, 1)
        self.assertEqual(len(Handler.requests), before + 1)
        self.assertIn("Ferry Console", result.stderr)

    def test_native_oauth_reads_and_write_gate(self) -> None:
        for path in ("/api/oauth/native/drivers", "/api/oauth/native/kimi/accounts/test/status", "/api/oauth/usage-reset-requests"):
            result = self.run_client("management", "GET", path)
            self.assertEqual(result.returncode, 0, result.stderr)
        for suffix in ("login", "complete"):
            before = len(Handler.requests)
            result = self.run_client("management", "POST", "/api/oauth/native/kimi/accounts/test/" + suffix, "--json", "{}")
            self.assertEqual(result.returncode, 2)
            self.assertEqual(len(Handler.requests), before)
            self.assertIn("--confirm-high-risk", result.stderr)

    def test_report_read_preserves_exact_scope(self) -> None:
        path = "/api/analytics?from=2026-09-01T00%3A00%3A00.123Z&to=2026-09-02T00%3A00%3A00.123Z&model=m1&provider=p1"
        result = self.run_client("management", "GET", path)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(Handler.requests[-1]["path"], path)
        self.assertEqual(Handler.requests[-1]["authorization"], "Bearer smt_test_secret")

    def test_session_only_and_legacy_routes_are_rejected_locally(self) -> None:
        before = len(Handler.requests)
        session_only = self.run_client("management", "GET", "/api/management-tokens")
        legacy = self.run_client("management", "GET", "/api/state")
        versioned = self.run_client("management", "GET", "/api/v2/access")
        auth = self.run_client("management", "GET", "/api/auth/status")
        self.assertEqual((session_only.returncode, legacy.returncode, versioned.returncode, auth.returncode), (2, 2, 2, 2))
        self.assertEqual(len(Handler.requests), before)

    def test_model_sync_requires_confirmation_before_publishing_models(self) -> None:
        before = len(Handler.requests)
        path = "/api/providers/test-provider/models/sync"
        result = self.run_client("management", "POST", path)
        self.assertEqual(result.returncode, 2)
        self.assertEqual(len(Handler.requests), before)
        self.assertIn("--confirm-high-risk", result.stderr)

        result = self.run_client("management", "POST", path, "--confirm-high-risk")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(Handler.requests), before + 1)
        self.assertEqual(Handler.requests[-1]["path"], path)
        self.assertEqual(Handler.requests[-1]["method"], "POST")

    def test_cross_origin_redirect_cannot_receive_management_token(self) -> None:
        before = len(Handler.requests)
        result = self.run_client("management", "GET", "/api/models/redirect")
        self.assertEqual(result.returncode, 2)
        self.assertIn("cross-origin redirect refused", result.stderr)
        self.assertEqual(len(Handler.requests), before + 1)
        self.assertEqual(Handler.requests[-1]["path"], "/api/models/redirect")

    def test_transport_headers_cannot_be_overridden(self) -> None:
        before = len(Handler.requests)
        result = self.run_client("management", "GET", "/api/access", "--header", "Host: attacker.invalid")
        self.assertEqual(result.returncode, 2)
        self.assertIn("cannot be overridden", result.stderr)
        self.assertEqual(len(Handler.requests), before)

    def test_multipart_image_edit_uses_inference_key(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            image = Path(directory, "input.png")
            image.write_bytes(b"PNG-BYTES")
            result = self.run_client("inference", "POST", "/v1/images/edits", "--form", "model=gpt-image-2", "--file", f"image={image}")
        self.assertEqual(result.returncode, 0, result.stderr)
        request = Handler.requests[-1]
        self.assertEqual(request["authorization"], "Bearer sk-ferry-test")
        self.assertTrue(str(request["content_type"]).startswith("multipart/form-data; boundary="))
        self.assertIn(b"PNG-BYTES", request["body"])
        self.assertIn(b'gpt-image-2', request["body"])


if __name__ == "__main__":
    unittest.main()
