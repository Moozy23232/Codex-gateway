"""A loopback HTTP/SSE Responses gateway, using only the Python standard library."""

from collections import OrderedDict
import hashlib
import hmac
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import math
import os
from pathlib import Path
import re
import socket
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

from .config import GatewayError, fingerprint, load_catalog, load_config, read_token, resolve_api_key

IDENTITY = "codex-gateway"
MAX_REQUEST_BYTES = 64 * 1024 * 1024
MAX_ERROR_BYTES = 1024 * 1024
MAX_COMPAT_RETRIES = 16
REQUEST_TIMEOUT = 30
UPSTREAM_TIMEOUT = 300
AUTH_ROTATION_GRACE = 120
HOP_HEADERS = {
    "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
    "te", "trailer", "transfer-encoding", "upgrade", "host", "content-length",
}
AUTH_HEADERS = {
    "authorization", "cookie", "cookie2", "x-api-key", "api-key",
    "x-codex-gateway-token", "x-codex-router-token",
}
# Third-party services receive only protocol headers. A denylist cannot recognize
# every client/vendor's private authentication or account-identification header.
API_HEADERS = {
    "accept", "accept-language", "content-type", "user-agent", "openai-beta",
    "x-codex-beta-features", "x-codex-turn-state", "x-codex-turn-metadata",
    "x-client-request-id", "x-request-id", "idempotency-key",
}


def rejected_reasoning_id(raw):
    """Recognize only an explicit encrypted-content error naming a reasoning ID."""
    def visit(value, depth=0):
        if depth > 8:
            return None
        if isinstance(value, str):
            try:
                return visit(json.loads(value.removeprefix("data:").strip()), depth + 1)
            except (ValueError, RecursionError):
                return None
        if isinstance(value, dict):
            if value.get("code") == "invalid_encrypted_content":
                match = re.search(
                    r"\bitem\s+['\"]?(rs_[A-Za-z0-9_-]+)",
                    str(value.get("message", "")), re.I,
                )
                return match.group(1) if match else None
            for child in value.values():
                found = visit(child, depth + 1)
                if found:
                    return found
        return None

    try:
        return visit(raw.decode("utf-8").strip())
    except (UnicodeError, RecursionError):
        return None


def reasoning_key(provider, item):
    if (isinstance(item, dict) and item.get("type") == "reasoning"
            and isinstance(item.get("id"), str)
            and isinstance(item.get("encrypted_content"), str)
            and item["encrypted_content"]):
        return provider, item["id"], hashlib.sha256(item["encrypted_content"].encode()).digest()
    return None


def _unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("Duplicate JSON member")
        result[key] = value
    return result


def _invalid_constant(_value):
    raise ValueError("Nonfinite JSON number")


def _finite_float(value):
    parsed = float(value)
    if not math.isfinite(parsed):
        raise ValueError("Nonfinite JSON number")
    return parsed


def _single_header(headers, name):
    values = headers.get_all(name, [])
    return values[0] if len(values) == 1 else None


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *_args, **_kwargs):
        return None


class Router(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, home: Path, port=None):
        self.home = Path(home)
        self.config = load_config(self.home)
        self.catalog = load_catalog(self.home)
        self.fingerprint = fingerprint(self.home)
        self.admin_token = read_token(self.home, "admin")
        self.client_token = (read_token(self.home, "client")
                             if self.config["client_auth"] == "token" else None)
        self.auth_path = Path(self.config["codex_home"]) / "auth.json"
        self.auth_lock = threading.Lock()
        self.auth_hashes = OrderedDict()
        self.activity_lock = threading.Lock()
        self.active = 0
        self.stopping = False
        self.rejected = OrderedDict()
        self.rejected_lock = threading.Lock()
        host = self.config["listen"]["host"]
        if ":" in host:
            self.address_family = socket.AF_INET6
        super().__init__((host, self.config["listen"]["port"] if port is None else port), Handler)

    def refresh_auth(self):
        """Allow a short overlap while Codex rotates OAuth credentials."""
        now = time.monotonic()
        try:
            auth = json.loads(self.auth_path.read_text(encoding="utf-8"))
            if not isinstance(auth, dict):
                auth = {}
            tokens = auth.get("tokens")
            access = tokens.get("access_token") if isinstance(tokens, dict) else None
            for token in (auth.get("OPENAI_API_KEY"), access):
                if isinstance(token, str) and token:
                    digest = hashlib.sha256(token.encode()).digest()
                    self.auth_hashes[digest] = now
                    self.auth_hashes.move_to_end(digest)
        except (OSError, ValueError):
            pass  # A credentials file may be replaced during OAuth refresh.
        for digest, last_seen in list(self.auth_hashes.items()):
            if now - last_seen > AUTH_ROTATION_GRACE:
                del self.auth_hashes[digest]
        while len(self.auth_hashes) > 4:
            self.auth_hashes.popitem(last=False)

    def authenticated(self, headers):
        value = _single_header(headers, "Authorization")
        if not value:
            return False
        scheme, separator, token = value.partition(" ")
        if scheme.lower() != "bearer" or not separator or not token or any(c.isspace() for c in token):
            return False
        supplied = hashlib.sha256(token.encode()).digest()
        if self.client_token is not None:
            return hmac.compare_digest(supplied, hashlib.sha256(self.client_token.encode()).digest())
        with self.auth_lock:
            self.refresh_auth()
            return any(hmac.compare_digest(supplied, digest) for digest in self.auth_hashes)

    def administrative(self, headers):
        value = _single_header(headers, "X-Codex-Gateway-Token")
        return value is not None and hmac.compare_digest(value.encode(), self.admin_token.encode())

    def handle_error(self, _request, _client_address):
        # The default traceback may contain upstream URLs or request data.
        # Handlers report expected errors to the client; do not log private data.
        pass


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = IDENTITY
    sys_version = ""

    def setup(self):
        self.request.settimeout(REQUEST_TIMEOUT)
        super().setup()

    def log_message(self, *_args):
        pass

    def reply(self, code, body):
        data = json.dumps(body, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        if self.close_connection:
            self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(data)

    def error(self, code, message):
        self.close_connection = True
        self.reply(code, {"error": {"type": "local_gateway_error", "message": message}})

    def request_path(self):
        try:
            parts = urllib.parse.urlsplit(self.path)
        except ValueError:
            self.error(400, "Invalid request target")
            return None
        if not self.path.startswith("/") or parts.netloc or parts.scheme or parts.fragment:
            self.error(400, "An origin-relative request target is required")
            return None
        return parts

    def do_GET(self):
        parts = self.request_path()
        if parts is None:
            return
        if parts.path == "/_gateway/health":
            if not self.server.administrative(self.headers):
                return self.error(403, "Gateway administration token required")
            with self.server.activity_lock:
                active = self.server.active
            return self.reply(200, {"service": IDENTITY, "fingerprint": self.server.fingerprint,
                                    "active_requests": active, "pid": os.getpid()})
        if not self.server.authenticated(self.headers):
            return self.error(401, "Gateway client credential required")
        if (parts.path in ("/v1/responses", "/responses")
                and self.headers.get("Upgrade", "").lower() == "websocket"):
            return self.error(426, "Use the Responses HTTP/SSE transport")
        if parts.path in ("/v1/models", "/models"):
            return self.reply(200, {**self.server.catalog, "object": "list", "data": [
                {"id": model["slug"], "object": "model"} for model in self.server.catalog["models"]
            ]})
        return self.error(404, "Unknown gateway endpoint")

    def do_POST(self):
        parts = self.request_path()
        if parts is None:
            return
        if parts.path == "/_gateway/shutdown":
            if not self.server.administrative(self.headers):
                return self.error(403, "Gateway administration token required")
            with self.server.activity_lock:
                if self.server.active:
                    return self.error(409, "Gateway has active requests; wait for them to finish")
                self.server.stopping = True
            # Always close: administrative clients do not need a reusable socket,
            # and any submitted request body must not become a second request.
            self.close_connection = True
            try:
                self.reply(200, {"stopping": True})
            finally:
                threading.Thread(target=self.server.shutdown, daemon=True).start()
            return
        if not self.server.authenticated(self.headers):
            return self.error(401, "Gateway client credential required")
        with self.server.activity_lock:
            if self.server.stopping:
                return self.error(503, "Gateway is stopping; retry after it restarts")
            self.server.active += 1
        try:
            self.forward(parts)
        except (BrokenPipeError, ConnectionResetError, TimeoutError):
            self.close_connection = True
        finally:
            with self.server.activity_lock:
                self.server.active -= 1

    def read_body(self):
        if self.headers.get_all("Transfer-Encoding"):
            self.error(411, "Content-Length is required; transfer-encoded requests are unsupported")
            return None
        encoding = _single_header(self.headers, "Content-Encoding")
        if self.headers.get_all("Content-Encoding") and (encoding or "").lower() != "identity":
            self.error(415, "Disable Codex request compression for the gateway")
            return None
        raw_length = _single_header(self.headers, "Content-Length")
        if raw_length is None:
            self.error(400 if self.headers.get_all("Content-Length") else 411,
                       "Exactly one Content-Length header is required")
            return None
        if not raw_length.isascii() or not raw_length.isdecimal():
            self.error(400, "Invalid Content-Length")
            return None
        # Limit digits before converting; malicious enormous numbers cannot make
        # int() raise outside the ordinary request error path.
        if len(raw_length) > 10 or not 1 <= int(raw_length) <= MAX_REQUEST_BYTES:
            self.error(413, "Request size must be between 1 byte and 64 MiB")
            return None
        try:
            length = int(raw_length)
            deadline = time.monotonic() + REQUEST_TIMEOUT
            chunks = []
            remaining = length
            while remaining:
                budget = deadline - time.monotonic()
                if budget <= 0:
                    raise TimeoutError
                self.connection.settimeout(budget)
                chunk = self.rfile.read1(min(remaining, 65536))
                if not chunk:
                    break
                chunks.append(chunk)
                remaining -= len(chunk)
            raw = b"".join(chunks)
            self.connection.settimeout(REQUEST_TIMEOUT)
            if len(raw) != length:
                self.error(400, "Incomplete request body")
                return None
            body = json.loads(raw.decode("utf-8"), object_pairs_hook=_unique_object,
                              parse_constant=_invalid_constant, parse_float=_finite_float)
            if not isinstance(body, dict) or not isinstance(body.get("model"), str) or not body["model"]:
                self.error(400, "A model name is required")
                return None
            return body
        except TimeoutError:
            self.error(408, "Timed out while reading the request body")
        except (ValueError, UnicodeError, RecursionError):
            self.error(400, "Invalid JSON request")
        return None

    def forward(self, parts):
        suffix = parts.path[3:] if parts.path.startswith("/v1/") else parts.path
        if suffix not in ("/responses", "/responses/compact", "/responses/input_tokens"):
            return self.error(404, "Unsupported Responses endpoint")
        if parts.query:
            return self.error(400, "Responses query parameters are unsupported; use the JSON body")
        body = self.read_body()
        if body is None:
            return
        route = self.server.config["models"].get(body["model"])
        if route is None:
            return self.error(400, "Unknown model alias")
        provider_name = route["provider"]
        provider = self.server.config["providers"][provider_name]
        body["model"] = route["model"]
        compatibility = self.server.config.get("retry_invalid_encrypted_reasoning", False)
        if compatibility and isinstance(body.get("input"), list):
            with self.server.rejected_lock:
                body["input"] = [item for item in body["input"]
                                 if reasoning_key(provider_name, item) not in self.server.rejected]
        connection_tokens = {token.strip().lower()
                             for token in self.headers.get("Connection", "").split(",")}
        omitted = HOP_HEADERS | AUTH_HEADERS | connection_tokens | {"accept-encoding", "expect"}
        headers = {key: value for key, value in self.headers.items() if key.lower() not in omitted}
        if provider["auth"] == "codex":
            if self.server.config["client_auth"] != "codex":
                return self.error(500, "Codex upstream authentication requires Codex client authentication")
            headers["Authorization"] = self.headers["Authorization"]
        else:
            headers = {key: value for key, value in headers.items() if key.lower() in API_HEADERS}
            try:
                api_key = resolve_api_key(provider)
                if not api_key or any(character in api_key for character in "\r\n"):
                    raise ValueError("Invalid API key")
                headers["Authorization"] = "Bearer " + api_key
            except (GatewayError, OSError, ValueError):
                return self.error(503, "Upstream API credential is unavailable")
        headers["Accept-Encoding"] = "identity"
        headers["Content-Type"] = "application/json"
        # ProxyHandler(proxy_map) still consults NO_PROXY and platform bypass
        # rules. Apply an explicitly configured proxy directly to each Request
        # so this provider's route is independent of the launcher's environment.
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
        url = provider["base_url"].rstrip("/") + suffix
        upstream = None
        buffered = b""
        try:
            for attempt in range(MAX_COMPAT_RETRIES + 1):
                request = urllib.request.Request(
                    url, data=json.dumps(body, ensure_ascii=False, allow_nan=False).encode(), headers=headers,
                )
                if provider.get("proxy"):
                    proxy = urllib.parse.urlsplit(provider["proxy"])
                    request.set_proxy(proxy.netloc, proxy.scheme)
                try:
                    upstream = opener.open(request, timeout=UPSTREAM_TIMEOUT)
                except urllib.error.HTTPError as error:
                    upstream = error
                if 300 <= upstream.status < 400:
                    upstream.close()
                    return self.error(502, "Upstream redirects are unsupported")
                if upstream.headers.get("Content-Encoding", "identity").lower() != "identity":
                    upstream.close()
                    return self.error(502, "Upstream returned an unsupported compressed response")
                buffered = b""
                if not compatibility or upstream.status != 400 or not isinstance(body.get("input"), list):
                    break
                buffered = upstream.read(MAX_ERROR_BYTES + 1)
                item_id = rejected_reasoning_id(buffered) if len(buffered) <= MAX_ERROR_BYTES else None
                rejected = [item for item in body["input"]
                            if reasoning_key(provider_name, item) and item["id"] == item_id]
                if not rejected or attempt == MAX_COMPAT_RETRIES:
                    break
                with self.server.rejected_lock:
                    for item in rejected:
                        self.server.rejected[reasoning_key(provider_name, item)] = None
                    while len(self.server.rejected) > 4096:
                        self.server.rejected.popitem(last=False)
                body["input"] = [item for item in body["input"] if item not in rejected]
                upstream.close()
        except (OSError, urllib.error.URLError, http.client.HTTPException, ValueError):
            if upstream is not None:
                upstream.close()
            return self.error(502, "Cannot complete the upstream request")
        self.stream_response(upstream, buffered)

    def stream_response(self, upstream, buffered):
        # A broken or timed-out stream is closed without a final chunk, so clients
        # see an interrupted response instead of a falsely successful completion.
        try:
            with upstream:
                self.connection.settimeout(UPSTREAM_TIMEOUT)
                self.send_response(upstream.status)
                connection_tokens = {token.strip().lower()
                                     for token in upstream.headers.get("Connection", "").split(",")}
                omitted = HOP_HEADERS | connection_tokens | {"set-cookie", "server", "date"}
                for key, value in upstream.headers.items():
                    if key.lower() not in omitted:
                        self.send_header(key, value)
                if upstream.status in (204, 205):
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return
                self.send_header("Transfer-Encoding", "chunked")
                self.send_header("X-Accel-Buffering", "no")
                self.end_headers()
                if buffered:
                    self.write_chunk(buffered)
                while chunk := upstream.read1(65536):
                    self.write_chunk(chunk)
                # read1 does not raise IncompleteRead for every short fixed-length
                # response. Preserve this failure rather than completing framing.
                if getattr(upstream, "length", None):
                    self.close_connection = True
                    return
                self.wfile.write(b"0\r\n\r\n")
                self.wfile.flush()
        except (OSError, http.client.HTTPException):
            self.close_connection = True
        finally:
            self.connection.settimeout(REQUEST_TIMEOUT)

    def write_chunk(self, data):
        self.wfile.write(f"{len(data):X}\r\n".encode() + data + b"\r\n")
        self.wfile.flush()


def serve(home: Path):
    """Run the gateway in the foreground until interrupted or shut down."""
    with Router(home) as server:
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass
