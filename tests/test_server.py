"""Socket-level checks against local fake Responses providers; no external calls."""

import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import socket
import tempfile
import threading
import time
import unittest
from unittest import mock
import urllib.error
import urllib.request

from codex_gateway import server as gateway


ADMIN_TOKEN = "test-admin-token-12345678901234567890"
CLIENT_TOKEN = "test-client-token-12345678901234567890"
FIRST_EVENT = (b'event: response.function_call_arguments.delta\n'
               b'data: {"delta":"{\\"path\\":\\"a"}\n\n')
LAST_EVENT = (b'event: response.function_call_arguments.delta\n'
              b'data: {"delta":".py\\"}"}\n\n'
              b'event: response.completed\ndata: {"type":"response.completed"}\n\n')


class Upstream(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_GET(self):
        self.server.requests.append((self.path, dict(self.headers), None))
        self.send_response(200)
        self.end_headers()

    def do_CONNECT(self):
        self.server.requests.append((self.path, dict(self.headers), None))
        # Recording the real tunnel request is enough to verify routing and
        # credential isolation without requiring a certificate fixture or DNS.
        self.send_response(502)
        self.end_headers()

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self.server.requests.append((self.path, dict(self.headers), body))
        if body.get("redirect"):
            self.send_response(307)
            self.send_header("Location", f"http://127.0.0.1:{self.server.server_port}/stolen")
            self.end_headers()
            return
        rejected = next((item for item in body.get("input", [])
                         if body.get("reject_id") and isinstance(item, dict)
                         and item.get("id") == body["reject_id"]), None)
        if rejected:
            self.send_response(body.get("reject_status", 400))
            self.end_headers()
            detail = {"error": {
                "code": body.get("error_code", "invalid_encrypted_content"),
                "message": f'The encrypted content for item {rejected["id"]} could not be verified.',
            }}
            wrapped = "data:" + json.dumps({"error": {"code": "400", "param": json.dumps(detail)}}) + "\n\n"
            self.wfile.write(wrapped.encode())
            return
        self.send_response(body.get("status", 200))
        self.send_header("Content-Type", "text/event-stream" if body.get("stream") else "application/json")
        self.send_header("Set-Cookie", "private-upstream-cookie=yes")
        if body.get("compressed"):
            self.send_header("Content-Encoding", "gzip")
        self.end_headers()
        if body.get("stream"):
            self.wfile.write(FIRST_EVENT)
            self.wfile.flush()
            self.server.finish.wait(5)
            self.wfile.write(LAST_EVENT)
        else:
            self.wfile.write(json.dumps({"seen": body}, ensure_ascii=False).encode())


class ServerTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="codex-gateway-test-")
        self.addCleanup(temporary.cleanup)
        self.home = Path(temporary.name)
        self.codex_home = self.home / "codex"
        self.codex_home.mkdir()
        (self.codex_home / "auth.json").write_text(json.dumps({"tokens": {"access_token": "official-token"}}))
        self.upstream = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
        self.upstream.daemon_threads = True
        self.upstream.requests = []
        self.upstream.finish = threading.Event()
        self.start(self.upstream)
        self.addCleanup(self.upstream.finish.set)
        endpoint = f"http://127.0.0.1:{self.upstream.server_port}/v1"
        self.config = {
            "version": 1, "listen": {"host": "127.0.0.1", "port": 33989},
            "codex_home": str(self.codex_home), "client_auth": "codex",
            "providers": {
                "official": {"base_url": endpoint, "auth": "codex"},
                "relay": {"base_url": endpoint, "auth": "api_key", "api_key_env": "GATEWAY_TEST_RELAY_KEY"},
            },
            "models": {
                "gpt-test": {"provider": "official", "model": "gpt-test", "template": "gpt-test"},
                "relay/gpt-test": {"provider": "relay", "model": "actual-model", "template": "gpt-test"},
            },
            "retry_invalid_encrypted_reasoning": False,
        }
        (self.home / "config.json").write_text(json.dumps(self.config))
        (self.home / "models.json").write_text(json.dumps({"models": [{"slug": slug} for slug in self.config["models"]]}))
        (self.home / "admin-token").write_text(ADMIN_TOKEN + "\n")
        (self.home / "client-token").write_text(CLIENT_TOKEN + "\n")
        environment = mock.patch.dict(os.environ, {"GATEWAY_TEST_RELAY_KEY": "relay-key"})
        environment.start()
        self.addCleanup(environment.stop)
        self.router = gateway.Router(self.home, port=0)
        self.start(self.router)
        self.base = f"http://127.0.0.1:{self.router.server_port}"
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def start(self, server):
        thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True)
        thread.start()
        self.addCleanup(self.stop, server, thread)
        return thread

    @staticmethod
    def stop(server, thread):
        server.shutdown()
        server.server_close()
        thread.join(2)

    def call(self, body=None, path="/v1/responses", headers=None, method=None):
        request = urllib.request.Request(
            self.base + path, data=json.dumps(body).encode() if body is not None else None,
            headers={"Authorization": "Bearer official-token", **(headers or {})}, method=method,
        )
        return self.opener.open(request, timeout=3)

    def assert_error(self, status, body=None, path="/v1/responses", headers=None, method=None):
        with self.assertRaises(urllib.error.HTTPError) as caught:
            self.call(body, path, headers, method)
        with caught.exception as response:
            self.assertEqual(response.code, status)
            return response.read()

    def test_alias_credentials_and_tool_fields(self):
        body = {
            "model": "relay/gpt-test",
            "input": [
                {"type": "reasoning", "id": "rs_opaque", "encrypted_content": "opaque-A=="},
                {"type": "function_call", "call_id": "tool-1", "name": "shell",
                 "arguments": '{"command":"printf \\\"中文\\\"","items":[1,2]}'},
                {"type": "function_call_output", "call_id": "tool-1", "output": "\nunchanged\n"},
            ],
            "tools": [{"type": "custom", "name": "patch"}],
            "previous_response_id": "resp_previous", "reasoning": {"effort": "high"}, "store": False,
        }
        private_headers = {
            "ChatGPT-Account-ID": "official-account", "OpenAI-Organization": "private-org",
            "OpenAI-Project": "private-project", "Cookie": "private-cookie",
            "X-Api-Key": "private-api-key", "X-Access-Token": "private-extra-token",
            "X-Codex-Gateway-Token": ADMIN_TOKEN, "Proxy-Authorization": "private-proxy-auth",
            "X-Other-Custom-Credential": "private-custom-credential", "OpenAI-Beta": "responses=v1",
        }
        with self.call(body, headers=private_headers) as response:
            self.assertEqual(json.load(response)["seen"], {**body, "model": "actual-model"})
            self.assertIsNone(response.headers.get("Set-Cookie"))
        _path, headers, _seen = self.upstream.requests[-1]
        lowered = {key.lower(): value for key, value in headers.items()}
        self.assertEqual(lowered["authorization"], "Bearer relay-key")
        self.assertEqual(lowered["openai-beta"], "responses=v1")
        for key in private_headers:
            if key != "OpenAI-Beta":
                self.assertNotIn(key.lower(), lowered)
        with self.call({"model": "gpt-test"}, headers={"ChatGPT-Account-ID": "official-account"}) as response:
            response.read()
        self.assertEqual(self.upstream.requests[-1][1]["Authorization"], "Bearer official-token")
        self.assertEqual(self.upstream.requests[-1][1]["Chatgpt-Account-Id"], "official-account")

    def test_stream_preserves_sse_bytes_and_arrives_before_completion(self):
        with self.call({"model": "gpt-test", "stream": True}) as response:
            self.assertEqual(response.read(len(FIRST_EVENT)), FIRST_EVENT)
            self.assertFalse(self.upstream.finish.is_set())
            self.upstream.finish.set()
            self.assertEqual(response.read(), LAST_EVENT)

    def test_catalog_and_websocket_fallback_require_client_authentication(self):
        self.assert_error(401, path="/v1/models", headers={"Authorization": "Bearer wrong"})
        with self.call(path="/v1/models") as response:
            catalog = json.load(response)
        self.assertEqual([model["id"] for model in catalog["data"]], list(self.config["models"]))
        self.assertEqual(catalog["models"][1]["slug"], "relay/gpt-test")
        self.assert_error(426, headers={"Upgrade": "websocket", "Connection": "Upgrade"})
        self.assertEqual(self.upstream.requests, [])

    def test_auth_rotation_and_openai_key(self):
        with self.call({"model": "gpt-test"}) as response:
            response.read()
        auth_path = self.codex_home / "auth.json"
        auth_path.write_text(json.dumps({"tokens": {"access_token": "new-token"}, "OPENAI_API_KEY": "api-token"}))
        for token in ("new-token", "api-token", "official-token"):
            with self.call({"model": "gpt-test"}, headers={"Authorization": "Bearer " + token}) as response:
                response.read()
        stale_time = time.monotonic() - gateway.AUTH_ROTATION_GRACE - 1
        for digest in self.router.auth_hashes:
            self.router.auth_hashes[digest] = stale_time
        self.assert_error(401, {"model": "gpt-test"})
        auth_path.write_text('{"tokens": null}')
        self.assert_error(401, {"model": "gpt-test"}, headers={"Authorization": "Bearer unknown"})

    def test_client_token_mode_rejects_codex_and_admin_credentials(self):
        self.config["client_auth"] = "token"
        del self.config["providers"]["official"]
        del self.config["models"]["gpt-test"]
        (self.home / "config.json").write_text(json.dumps(self.config))
        token_router = gateway.Router(self.home, port=0)
        self.start(token_router)
        self.base = f"http://127.0.0.1:{token_router.server_port}"
        for token in ("official-token", ADMIN_TOKEN, "wrong"):
            self.assert_error(401, {"model": "relay/gpt-test"}, headers={"Authorization": "Bearer " + token})
        with self.call({"model": "relay/gpt-test"}, headers={"Authorization": "Bearer " + CLIENT_TOKEN}) as response:
            response.read()
        self.assertEqual(self.upstream.requests[-1][1]["Authorization"], "Bearer relay-key")

    def test_environment_proxy_is_ignored_and_explicit_proxy_is_used(self):
        poisoned = {"http_proxy": "http://127.0.0.1:1", "HTTP_PROXY": "http://127.0.0.1:1",
                    "https_proxy": "http://127.0.0.1:1", "HTTPS_PROXY": "http://127.0.0.1:1",
                    "ALL_PROXY": "http://127.0.0.1:1", "all_proxy": "http://127.0.0.1:1",
                    "no_proxy": "", "NO_PROXY": ""}
        with mock.patch.dict(os.environ, poisoned):
            with self.call({"model": "relay/gpt-test"}) as response:
                response.read()
            relay = self.router.config["providers"]["relay"]
            relay["base_url"] = "http://provider.invalid/v1"
            relay["proxy"] = f"http://127.0.0.1:{self.upstream.server_port}"
            with mock.patch.dict(os.environ, {"NO_PROXY": "*", "no_proxy": "*"}):
                with self.call({"model": "relay/gpt-test"}) as response:
                    response.read()
        self.assertEqual(self.upstream.requests[-1][0], "http://provider.invalid/v1/responses")

    def test_explicit_proxy_uses_https_connect_despite_no_proxy(self):
        relay = self.router.config["providers"]["relay"]
        relay["base_url"] = "https://provider.invalid/v1"
        relay["proxy"] = f"http://127.0.0.1:{self.upstream.server_port}"
        with mock.patch.dict(os.environ, {"NO_PROXY": "*", "no_proxy": "*"}):
            self.assert_error(502, {"model": "relay/gpt-test"})
        self.assertEqual(len(self.upstream.requests), 1)
        path, headers, body = self.upstream.requests[0]
        self.assertEqual(path, "provider.invalid:443")
        self.assertIsNone(body)
        self.assertNotIn("authorization", {key.lower() for key in headers})

    def test_redirects_and_unexpected_compression_are_rejected(self):
        self.assert_error(502, {"model": "relay/gpt-test", "redirect": True})
        self.assertEqual(len(self.upstream.requests), 1)
        self.assert_error(502, {"model": "relay/gpt-test", "compressed": True})

    def test_missing_provider_credential_and_unreachable_upstream(self):
        with mock.patch.dict(os.environ, {"GATEWAY_TEST_RELAY_KEY": ""}):
            self.assert_error(503, {"model": "relay/gpt-test"})
        self.assertEqual(self.upstream.requests, [])
        self.router.config["providers"]["relay"]["base_url"] = "http://127.0.0.1:1/v1"
        self.assert_error(502, {"model": "relay/gpt-test"})

    def test_only_responses_endpoints_and_valid_uncompressed_json_are_accepted(self):
        self.assert_error(400, {"model": "missing"})
        self.assert_error(404, {"model": "gpt-test"}, path="/v1/chat/completions")
        self.assert_error(400, {"model": "gpt-test"}, path="/v1/responses?api_key=private")
        self.assert_error(415, {"model": "gpt-test"}, headers={"Content-Encoding": "zstd"})
        for raw in (b'[]', b'{"model": "gpt-test", "model": "other"}',
                    b'{"model":"gpt-test","input":NaN}', b'{"model":"gpt-test","number":1e9999}',
                    b'{"model":"\xff"}'):
            connection = http.client.HTTPConnection("127.0.0.1", self.router.server_port, timeout=3)
            try:
                connection.request("POST", "/v1/responses", raw, {"Authorization": "Bearer official-token"})
                response = connection.getresponse()
                self.assertEqual(response.status, 400)
                response.read()
            finally:
                connection.close()
        self.assertEqual(self.upstream.requests, [])
        for path in ("/v1/responses/compact", "/v1/responses/input_tokens", "/responses"):
            with self.call({"model": "gpt-test"}, path=path) as response:
                response.read()
            self.assertTrue(self.upstream.requests[-1][0].endswith(path.removeprefix("/v1")))

    def test_duplicate_auth_and_length_headers_are_rejected(self):
        for duplicate, status in (("Authorization: Bearer official-token\r\n", 401),
                                  ("Content-Length: 20\r\n", 400)):
            request = ("POST /v1/responses HTTP/1.1\r\nHost: localhost\r\n"
                       "Authorization: Bearer official-token\r\nContent-Length: 20\r\n" + duplicate
                       + '\r\n{"model":"gpt-test"}')
            with socket.create_connection(("127.0.0.1", self.router.server_port), timeout=3) as connection:
                connection.sendall(request.encode())
                response = http.client.HTTPResponse(connection)
                response.begin()
                self.assertEqual(response.status, status)
                response.read()
        self.assertEqual(self.upstream.requests, [])

    def test_request_body_read_has_a_deadline(self):
        with mock.patch.object(gateway, "REQUEST_TIMEOUT", 0.05):
            with socket.create_connection(("127.0.0.1", self.router.server_port), timeout=3) as connection:
                connection.sendall(b"POST /v1/responses HTTP/1.1\r\nHost: localhost\r\n"
                                   b"Authorization: Bearer official-token\r\nContent-Length: 100\r\n\r\n{")
                response = http.client.HTTPResponse(connection)
                response.begin()
                self.assertEqual(response.status, 408)
                response.read()
        self.assertEqual(self.upstream.requests, [])

    def test_compatibility_is_opt_in_and_preserves_compaction(self):
        reasoning = {"type": "reasoning", "id": "rs_rejected", "encrypted_content": "opaque-rejected"}
        compaction = {"type": "compaction", "id": "rs_summary", "encrypted_content": "opaque-summary"}
        body = {"model": "relay/gpt-test", "input": [reasoning, compaction], "reject_id": "rs_rejected"}
        self.assertIn(b"invalid_encrypted_content", self.assert_error(400, body))
        self.assertEqual(len(self.upstream.requests), 1)
        self.router.config["retry_invalid_encrypted_reasoning"] = True
        with self.call(body) as response:
            self.assertEqual(json.load(response)["seen"]["input"], [compaction])
        self.assertEqual(len(self.upstream.requests), 3)
        with self.call(body) as response:
            self.assertEqual(json.load(response)["seen"]["input"], [compaction])
        self.assertEqual(len(self.upstream.requests), 4)
        with self.call({**body, "model": "gpt-test", "reject_id": "none"}) as response:
            self.assertEqual(json.load(response)["seen"]["input"], [reasoning, compaction])
        updated = {**reasoning, "encrypted_content": "new-provider-compatible-content"}
        with self.call({**body, "input": [updated, compaction], "reject_id": "none"}) as response:
            self.assertEqual(json.load(response)["seen"]["input"], [updated, compaction])
        self.assertEqual(body["input"], [reasoning, compaction])

    def test_compatibility_requires_exact_400_reasoning_rejection(self):
        self.router.config["retry_invalid_encrypted_reasoning"] = True
        reasoning = {"type": "reasoning", "id": "rs_bad", "encrypted_content": "opaque"}
        for item, extra, status in [
            ({"type": "compaction", "id": "rs_bad", "encrypted_content": "summary"}, {}, 400),
            ({"type": "message", "id": "rs_bad", "content": "text"}, {}, 400),
            (reasoning, {"error_code": "rate_limit_exceeded"}, 400),
            (reasoning, {"reject_status": 403}, 403),
        ]:
            self.assert_error(status, {"model": "relay/gpt-test", "reject_id": "rs_bad", "input": [item], **extra})
        self.assertEqual(len(self.upstream.requests), 4)
        self.assertIsNone(gateway.rejected_reasoning_id(b'{"error":{"code":"invalid_encrypted_content","message":"bad payload"}}'))

    def test_admin_auth_health_active_requests_and_shutdown(self):
        self.assert_error(403, path="/_gateway/health")
        self.assert_error(403, {}, path="/_gateway/shutdown")
        admin = {"X-Codex-Gateway-Token": ADMIN_TOKEN}
        with self.call({"model": "gpt-test", "stream": True}) as stream:
            self.assertEqual(stream.read(len(FIRST_EVENT)), FIRST_EVENT)
            with self.call(path="/_gateway/health", headers=admin) as response:
                health = json.load(response)
            self.assertEqual(health["service"], gateway.IDENTITY)
            self.assertEqual(health["pid"], os.getpid())
            self.assertEqual(health["fingerprint"], self.router.fingerprint)
            self.assertEqual(health["active_requests"], 1)
            self.assert_error(409, {}, path="/_gateway/shutdown", headers=admin)
            self.assertFalse(self.router.stopping)
            self.upstream.finish.set()
            self.assertEqual(stream.read(), LAST_EVENT)
        deadline = time.monotonic() + 2
        while self.router.active and time.monotonic() < deadline:
            time.sleep(0.005)
        with self.call({}, path="/_gateway/shutdown", headers=admin) as response:
            self.assertEqual(json.load(response), {"stopping": True})
        self.assertTrue(self.router.stopping)


if __name__ == "__main__":
    unittest.main()
