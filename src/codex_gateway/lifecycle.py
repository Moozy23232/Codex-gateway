"""Manage this gateway and launch Codex without changing its saved configuration."""

from __future__ import annotations

import contextlib
import errno
from http.client import HTTPException
import ipaddress
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
from urllib.error import HTTPError, URLError
from urllib.request import ProxyHandler, Request, build_opener

from . import config
from .server import IDENTITY, NoRedirect


_START_TIMEOUT = 10.0
_STOP_TIMEOUT = 5.0
_POLL_INTERVAL = 0.1


def _endpoint(value: dict) -> tuple[str, int]:
    host, port = value.get("host"), value.get("port")
    try:
        is_loopback = host == "localhost" or ipaddress.ip_address(host).is_loopback
    except (ValueError, TypeError):
        is_loopback = False
    if not is_loopback or type(port) is not int or not 1 <= port <= 65535:
        raise config.GatewayError("Gateway runtime contains an invalid loopback address.")
    return host, port


def _url(endpoint: tuple[str, int]) -> str:
    host, port = endpoint
    return f"http://{'[' + host + ']' if ':' in host else host}:{port}"


def _runtime(home: Path) -> Path:
    runtime = home / "runtime"
    runtime.mkdir(parents=True, exist_ok=True, mode=0o700)
    runtime.chmod(0o700)
    return runtime


def _private_fd(path: Path, flags: int) -> int:
    fd = os.open(path, flags | getattr(os, "O_NOFOLLOW", 0), 0o600)
    os.fchmod(fd, 0o600)
    return fd


@contextlib.contextmanager
def _locked(home: Path):
    # An advisory file lock is released by the OS if the launcher exits or crashes.
    import fcntl

    with os.fdopen(_private_fd(_runtime(home) / "lifecycle.lock", os.O_RDWR | os.O_CREAT), "a+") as handle:
        fcntl.flock(handle, fcntl.LOCK_EX)
        try:
            yield
        finally:
            fcntl.flock(handle, fcntl.LOCK_UN)


def _read_state(home: Path) -> dict | None:
    try:
        value = json.loads((home / "runtime" / "server.json").read_text())
    except FileNotFoundError:
        return None
    except (OSError, ValueError) as exc:
        raise config.GatewayError("Cannot read gateway runtime/server.json; check the local runtime state.") from exc
    if not isinstance(value, dict):
        raise config.GatewayError("Gateway runtime/server.json must contain an object.")
    _endpoint(value)
    return value


def _write_state(home: Path, endpoint: tuple[str, int], health: dict) -> None:
    runtime = _runtime(home)
    value = {"host": endpoint[0], "port": endpoint[1], "pid": health["pid"], "fingerprint": health["fingerprint"]}
    with tempfile.NamedTemporaryFile(mode="w", dir=runtime, prefix="server-", suffix=".json", delete=False) as handle:
        temporary = Path(handle.name)
        try:
            os.fchmod(handle.fileno(), 0o600)
            json.dump(value, handle)
            handle.write("\n")
        except BaseException:
            temporary.unlink(missing_ok=True)
            raise
    try:
        temporary.replace(runtime / "server.json")
    finally:
        temporary.unlink(missing_ok=True)


def _health(endpoint: tuple[str, int], token: str) -> dict | None:
    address = _url(endpoint)
    request = Request(address + "/_gateway/health", headers={"X-Codex-Gateway-Token": token})
    try:
        # Local lifecycle traffic must never go through an inherited HTTP proxy.
        with build_opener(ProxyHandler({}), NoRedirect()).open(request, timeout=1.0) as response:
            payload = response.read(65537)
        if len(payload) > 65536:
            raise ValueError("oversized health response")
        value = json.loads(payload)
    except HTTPError as exc:
        exc.close()
        raise config.GatewayError(
            f"Cannot verify ownership of the service at {address} (HTTP {exc.code}); leaving it unchanged."
        ) from None
    except URLError as exc:
        reason = exc.reason
        if isinstance(reason, OSError) and reason.errno in {errno.ECONNREFUSED, 10061}:
            return None
        raise config.GatewayError(f"The service at {address} did not answer the gateway health check; leaving it unchanged.") from None
    except (OSError, ValueError, HTTPException) as exc:
        raise config.GatewayError(f"The service at {address} did not provide a valid gateway health response; leaving it unchanged.") from exc
    if (
        not isinstance(value, dict)
        or value.get("service") != IDENTITY
        or not isinstance(value.get("fingerprint"), str)
        or type(value.get("active_requests")) is not int
        or value["active_requests"] < 0
        or type(value.get("pid")) is not int
        or value["pid"] <= 0
    ):
        raise config.GatewayError(f"The service at {address} is not this gateway; leaving it unchanged.")
    return {key: value[key] for key in ("service", "fingerprint", "active_requests", "pid")}


def _locate(home: Path, cfg: dict, token: str) -> tuple[tuple[str, int], dict | None]:
    desired = _endpoint(cfg["listen"])
    previous = _read_state(home)
    if previous is not None:
        recorded = _endpoint(previous)
        health = _health(recorded, token)
        if health is not None or recorded == desired:
            return recorded, health
    return desired, _health(desired, token)


def _result(endpoint: tuple[str, int], health: dict | None) -> dict:
    if health is None:
        return {"running": False}
    return {"running": True, **health, "url": _url(endpoint)}


def status(home: Path) -> dict:
    """Report only an authenticated gateway; never trust a PID file alone."""
    home = Path(home).expanduser().resolve()
    cfg = config.load_config(home)
    endpoint, health = _locate(home, cfg, config.read_token(home))
    return _result(endpoint, health)


def _shutdown(endpoint: tuple[str, int], token: str, health: dict) -> None:
    if health["active_requests"]:
        raise config.GatewayError("Gateway has active requests; let them finish before stopping or restarting it.")
    address = _url(endpoint)
    request = Request(address + "/_gateway/shutdown", data=b"", method="POST", headers={"X-Codex-Gateway-Token": token})
    try:
        with build_opener(ProxyHandler({}), NoRedirect()).open(request, timeout=2.0) as response:
            response.read(65536)
    except HTTPError as exc:
        exc.close()
        if exc.code == 409:
            raise config.GatewayError("Gateway became busy; let active requests finish before stopping or restarting it.") from None
        raise config.GatewayError(f"Gateway refused shutdown (HTTP {exc.code}); no process was killed.") from None
    except (OSError, URLError, HTTPException):
        raise config.GatewayError("Gateway did not confirm shutdown; no process was killed.") from None
    deadline = time.monotonic() + _STOP_TIMEOUT
    while time.monotonic() < deadline:
        try:
            with socket.create_connection(endpoint, timeout=0.5):
                pass
        except OSError as exc:
            if exc.errno in {errno.ECONNREFUSED, 10061}:
                return
        time.sleep(_POLL_INTERVAL)
    raise config.GatewayError("Gateway did not stop within five seconds; no process was killed.")


def stop(home: Path) -> dict:
    """Stop an idle gateway through its authenticated control API."""
    home = Path(home).expanduser().resolve()
    with _locked(home):
        cfg = config.load_config(home)
        token = config.read_token(home)
        endpoint, health = _locate(home, cfg, token)
        if health is not None:
            _shutdown(endpoint, token, health)
        (home / "runtime" / "server.json").unlink(missing_ok=True)
        return {"running": False}


def _cancel_start(process: subprocess.Popen) -> None:
    # This handle belongs to the child we just created. Never signal a PID read
    # from disk: it may have been reused after a machine restart.
    if process.poll() is None:
        try:
            process.terminate()
        except OSError:
            return
        try:
            process.wait(timeout=2)
        except subprocess.TimeoutExpired:
            pass


def start(home: Path) -> dict:
    """Reuse a matching gateway or restart an idle one after config changes."""
    home = Path(home).expanduser().resolve()
    with _locked(home):
        cfg = config.load_config(home)
        token = config.read_token(home)
        desired = _endpoint(cfg["listen"])
        fingerprint = config.fingerprint(home)
        endpoint, health = _locate(home, cfg, token)
        if health is not None:
            if endpoint == desired and health["fingerprint"] == fingerprint:
                _write_state(home, endpoint, health)
                return _result(endpoint, health)
            if health["active_requests"]:
                raise config.GatewayError("Gateway configuration changed while requests are active; retry after they finish.")
            if endpoint != desired and _health(desired, token) is not None:
                raise config.GatewayError("The new gateway port is already occupied; the existing gateway was left running.")
            _shutdown(endpoint, token, health)
        log_path = _runtime(home) / "server.log"
        server_env = os.environ.copy()
        package_root = str(Path(__file__).resolve().parents[1])
        server_env["PYTHONPATH"] = package_root + (os.pathsep + server_env["PYTHONPATH"] if server_env.get("PYTHONPATH") else "")
        try:
            with os.fdopen(_private_fd(log_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND), "ab", buffering=0) as log:
                process = subprocess.Popen(
                    [sys.executable, "-m", "codex_gateway", "--home", str(home), "serve"],
                    stdin=subprocess.DEVNULL,
                    stdout=log,
                    stderr=subprocess.STDOUT,
                    start_new_session=True,
                    close_fds=True,
                    env=server_env,
                )
        except OSError:
            raise config.GatewayError(f"Could not launch the gateway; check Python and write access to {log_path}.") from None
        try:
            deadline = time.monotonic() + _START_TIMEOUT
            while time.monotonic() < deadline:
                code = process.poll()
                if code is not None:
                    raise config.GatewayError(f"Gateway exited during startup (exit {code}); inspect {log_path} locally for details.")
                health = _health(desired, token)
                if health is not None:
                    if health["fingerprint"] != fingerprint or health["pid"] != process.pid:
                        raise config.GatewayError("Another process claimed the gateway port during startup; leaving that service unchanged.")
                    _write_state(home, desired, health)
                    return _result(desired, health)
                time.sleep(_POLL_INTERVAL)
            raise config.GatewayError(f"Gateway startup timed out; inspect {log_path} locally for details.")
        except BaseException:
            _cancel_start(process)
            raise


def _codex_env(home: Path, cfg: dict) -> dict[str, str]:
    env = os.environ.copy()
    env["CODEX_HOME"] = str(cfg["codex_home"])
    bypass = []
    for item in (env.get("NO_PROXY", "") + "," + env.get("no_proxy", "") + ",localhost,127.0.0.1,::1," + cfg["listen"]["host"]).split(","):
        item = item.strip()
        if item and item not in bypass:
            bypass.append(item)
    env["NO_PROXY"] = env["no_proxy"] = ",".join(bypass)
    if cfg["client_auth"] == "token":
        env["CODEX_GATEWAY_TOKEN"] = config.read_token(home, "client")
    else:
        proxy = env.get("all_proxy", env.get("ALL_PROXY"))
        if proxy is None:
            proxy = cfg.get("bootstrap_proxy") or next(
                (provider["proxy"] for provider in cfg["providers"].values() if provider["auth"] == "codex" and provider.get("proxy")),
                None,
            )
        if proxy is not None:
            for key in ("http_proxy", "https_proxy", "all_proxy"):
                upper = key.upper()
                if key not in env and upper not in env:
                    env[key] = env[upper] = proxy
                elif key not in env:
                    env[key] = env[upper]
                elif upper not in env:
                    env[upper] = env[key]
    return env


def _config_arguments(args: list[str]) -> tuple[list[str], list[str]]:
    """Keep all config overrides at one CLI level without inspecting prompt text."""
    overrides, remaining = [], []
    index = 0
    while index < len(args):
        argument = args[index]
        if argument == "--":
            remaining.extend(args[index:])
            break
        if argument in ("-c", "--config"):
            if index + 1 == len(args) or args[index + 1] == "--":
                raise config.GatewayError(f"{argument} requires a key=value argument before --.")
            overrides.append(args[index + 1])
            index += 2
            continue
        if argument.startswith("--config="):
            overrides.append(argument[len("--config="):])
        elif argument.startswith("-c") and not argument.startswith("--"):
            overrides.append(argument[2:].removeprefix("="))
        else:
            remaining.append(argument)
        index += 1
    return overrides, remaining


def run_codex(home: Path, args: list[str], codex_executable: str | None = None) -> int:
    """Launch the existing Codex executable with per-invocation routing options."""
    home = Path(home).expanduser().resolve()
    cfg = config.load_config(home)
    default_model = cfg.get("default_model") or next(iter(cfg["models"]), None)
    if default_model is None:
        raise config.GatewayError("No gateway models are configured; add a provider and model before launching Codex.")
    executable = shutil.which(str(codex_executable or "codex"))
    if not executable:
        raise config.GatewayError("Cannot find an executable Codex CLI; install Codex or specify its executable path.")
    user_overrides, remaining_args = _config_arguments(args)
    start(home)
    overrides = [
        "model_catalog_json=" + json.dumps(str(home / "models.json")),
        "features.enable_request_compression=false",
    ]
    base_url = _url(_endpoint(cfg["listen"])) + "/v1"
    if cfg["client_auth"] == "codex":
        overrides.extend(['model_provider="openai"', "openai_base_url=" + json.dumps(base_url)])
    else:
        overrides.extend([
            'model_provider="codex-gateway"',
            'model_providers.codex-gateway.name="Codex Gateway"',
            "model_providers.codex-gateway.base_url=" + json.dumps(base_url),
            'model_providers.codex-gateway.wire_api="responses"',
            'model_providers.codex-gateway.env_key="CODEX_GATEWAY_TOKEN"',
            "model_providers.codex-gateway.requires_openai_auth=false",
        ])
    overrides.append("model=" + json.dumps(default_model))
    # Codex 0.159.2 replaces the global -c list if a subcommand also has -c.
    # Put user overrides after our defaults, at the same global CLI level.
    overrides.extend(user_overrides)
    command = [executable]
    for override in overrides:
        command.extend(["-c", override])
    command.extend(remaining_args)
    try:
        child = subprocess.Popen(command, env=_codex_env(home, cfg))
    except OSError:
        raise config.GatewayError("Unable to start the Codex executable; check its path and permissions.") from None

    def terminate_child(*_):
        try:
            child.terminate()
        except ProcessLookupError:
            pass

    # Keep the existing terminal process group. Ctrl-C reaches Codex (and any
    # existing backup wrapper), while this launcher continues waiting for it.
    old_int = signal.signal(signal.SIGINT, lambda *_: None)
    old_term = signal.signal(signal.SIGTERM, terminate_child)
    try:
        result = child.wait()
    finally:
        signal.signal(signal.SIGTERM, old_term)
        signal.signal(signal.SIGINT, old_int)
    return result if result >= 0 else 128 - result
