import contextlib
from concurrent.futures import ThreadPoolExecutor
import copy
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock

from codex_gateway import config, lifecycle


class ControlHandler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def respond(self, code, value):
        body = json.dumps(value).encode()
        self.send_response(code)
        self.send_header("Content-Length", str(len(body)))
        if 300 <= code < 400 and self.server.redirect_to:
            self.send_header("Location", self.server.redirect_to)
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self.server.seen_tokens.append(self.headers.get("X-Codex-Gateway-Token"))
        self.respond(self.server.health_code, self.server.health)

    def do_POST(self):
        self.server.shutdown_calls += 1
        self.respond(self.server.shutdown_code, {"stopping": self.server.shutdown_code == 200})
        if self.server.shutdown_code == 200:
            def close_server():
                self.server.shutdown()
                self.server.server_close()
            threading.Thread(target=close_server, daemon=True).start()


class LifecycleTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.home = Path(self.temporary.name) / "gateway"
        self.home.mkdir()
        self.codex_home = Path(self.temporary.name) / "codex"
        self.codex_home.mkdir()
        self.original_config = b'model = "existing"\nmodel_provider = "my-provider"\n'
        (self.codex_home / "config.toml").write_bytes(self.original_config)
        (self.codex_home / "auth.json").write_text('{"untouched":true}')
        self.cfg = {
            "listen": {"host": "127.0.0.1", "port": 33989},
            "codex_home": str(self.codex_home),
            "client_auth": "codex",
            "providers": {"official": {"auth": "codex", "base_url": "https://official.invalid/backend-api/codex", "proxy": "http://127.0.0.1:8080"}},
            "models": {"official/model": {"provider": "official", "model": "model", "template": "model"}},
            "default_model": "official/model",
            "bootstrap_proxy": None,
        }
        self.health = {"service": "codex-gateway", "fingerprint": "current", "active_requests": 0, "pid": 12345}
        self.patch_load = mock.patch.object(config, "load_config", side_effect=lambda _: copy.deepcopy(self.cfg))
        self.patch_token = mock.patch.object(config, "read_token", side_effect=lambda _, name="admin": name + "-secret-value")
        self.patch_fingerprint = mock.patch.object(config, "fingerprint", return_value="current")
        for patch in (self.patch_load, self.patch_token, self.patch_fingerprint):
            patch.start()
            self.addCleanup(patch.stop)

    @contextlib.contextmanager
    def control_server(self, *, health=None, health_code=200, shutdown_code=200, redirect_to=None):
        server = ThreadingHTTPServer(("127.0.0.1", 0), ControlHandler)
        server.health = health if health is not None else copy.deepcopy(self.health)
        server.health_code = health_code
        server.shutdown_code = shutdown_code
        server.shutdown_calls = 0
        server.seen_tokens = []
        server.redirect_to = redirect_to
        thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.02}, daemon=True)
        thread.start()
        try:
            yield server
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)

    def use_server(self, server):
        self.cfg["listen"]["port"] = server.server_port

    def launch_codex(self, args=(), environment=None):
        child = mock.Mock()
        child.wait.return_value = 37
        with (
            mock.patch.dict(os.environ, environment or {}, clear=True),
            mock.patch.object(lifecycle, "start", return_value={"running": True}) as start,
            mock.patch.object(lifecycle.shutil, "which", return_value="/existing/bin/codex") as which,
            mock.patch.object(lifecycle.subprocess, "Popen", return_value=child) as run,
        ):
            before = dict(os.environ)
            result = lifecycle.run_codex(self.home, list(args))
            self.assertEqual(result, 37)
            self.assertEqual(dict(os.environ), before)
        start.assert_called_once_with(self.home)
        which.assert_called_once_with("codex")
        command = run.call_args.args[0]
        env = run.call_args.kwargs["env"]
        self.assertEqual((self.codex_home / "config.toml").read_bytes(), self.original_config)
        self.assertEqual((self.codex_home / "auth.json").read_text(), '{"untouched":true}')
        return command, env

    def test_codex_launch_keeps_existing_wrapper_and_config_and_user_overrides(self):
        user_args = ["resume", "session-id", "-c", 'model="user-choice"']
        command, env = self.launch_codex(user_args, {"NO_PROXY": "internal.invalid", "no_proxy": "custom.invalid"})
        self.assertEqual(command[0], "/existing/bin/codex")
        self.assertEqual(command[-2:], ["resume", "session-id"])
        self.assertLess(command.index('model="user-choice"'), command.index("resume"))
        self.assertIn('model_provider="openai"', command)
        self.assertIn('openai_base_url="http://127.0.0.1:33989/v1"', command)
        self.assertIn("features.enable_request_compression=false", command)
        self.assertIn("model_catalog_json=" + json.dumps(str(self.home / "models.json")), command)
        self.assertLess(command.index('model="official/model"'), command.index('model="user-choice"'))
        self.assertEqual(env["CODEX_HOME"], str(self.codex_home))
        self.assertEqual(set(env["NO_PROXY"].split(",")), {"internal.invalid", "custom.invalid", "localhost", "127.0.0.1", "::1"})
        self.assertEqual(env["NO_PROXY"], env["no_proxy"])
        self.assertNotIn("CODEX_GATEWAY_TOKEN", env)
        for key in ("http_proxy", "https_proxy", "all_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"):
            self.assertEqual(env[key], "http://127.0.0.1:8080")

    def test_subcommand_config_forms_share_global_level_and_preserve_order(self):
        user_args = [
            "-c", 'model="first"', "exec", "--json",
            "--config", 'model_reasoning_effort="low"',
            '--config=model="last"', '-cfeatures.example_one=true', '-c=features.example_two=false',
            "Explain the text --config=sample and -c sample without changing it.",
        ]
        original = list(user_args)
        command, _ = self.launch_codex(user_args)
        expected = [
            'model="first"', 'model_reasoning_effort="low"', 'model="last"',
            'features.example_one=true', 'features.example_two=false',
        ]
        extracted = command[1:command.index("exec")]
        self.assertEqual(extracted[-2 * len(expected):], [part for value in expected for part in ("-c", value)])
        self.assertEqual(command[command.index("exec"):], ["exec", "--json", original[-1]])
        self.assertEqual(user_args, original)
        self.assertIn("model_catalog_json=" + json.dumps(str(self.home / "models.json")), extracted)
        self.assertIn('openai_base_url="http://127.0.0.1:33989/v1"', extracted)

    def test_literal_argument_boundary_preserves_config_like_prompt_arguments(self):
        literal_args = ["--config", "literal text", "-c", 'model="literal"', '-cmodel="also literal"']
        command, _ = self.launch_codex(["exec", '-cmodel_reasoning_effort="low"', "--", *literal_args])
        self.assertEqual(command[command.index("exec"):], ["exec", "--", *literal_args])
        self.assertLess(command.index('model_reasoning_effort="low"'), command.index("exec"))
        self.assertNotIn('model="literal"', command[:command.index("exec")])

    def test_missing_config_value_fails_before_starting_gateway(self):
        for arguments in (["exec", "-c"], ["exec", "--config", "--", "prompt"]):
            with self.subTest(arguments=arguments), mock.patch.object(lifecycle, "start") as start, mock.patch.object(lifecycle.shutil, "which", return_value="/existing/bin/codex"):
                with self.assertRaisesRegex(config.GatewayError, "requires a key=value"):
                    lifecycle.run_codex(self.home, arguments)
                start.assert_not_called()

    def test_bootstrap_proxy_fills_gaps_and_preserves_explicit_environment(self):
        self.cfg["bootstrap_proxy"] = "http://127.0.0.1:9191"
        _, env = self.launch_codex(environment={"HTTPS_PROXY": "http://explicit.invalid:99", "http_proxy": "", "all_proxy": "http://lower.invalid:80", "ALL_PROXY": "http://upper.invalid:80"})
        self.assertEqual(env["https_proxy"], "http://explicit.invalid:99")
        self.assertEqual(env["HTTPS_PROXY"], "http://explicit.invalid:99")
        self.assertEqual(env["http_proxy"], "")
        self.assertEqual(env["HTTP_PROXY"], "")
        self.assertEqual(env["all_proxy"], "http://lower.invalid:80")
        self.assertEqual(env["ALL_PROXY"], "http://upper.invalid:80")
        _, clean_env = self.launch_codex()
        self.assertEqual(clean_env["HTTPS_PROXY"], self.cfg["bootstrap_proxy"])

    def test_all_proxy_is_the_fallback_before_configured_bootstrap_proxy(self):
        self.cfg["bootstrap_proxy"] = "http://127.0.0.1:9191"
        for key in ("ALL_PROXY", "all_proxy"):
            with self.subTest(variable=key):
                _, env = self.launch_codex(environment={key: "http://preferred.invalid:80"})
                for scheme in ("http_proxy", "https_proxy", "all_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"):
                    self.assertEqual(env[scheme], "http://preferred.invalid:80")

    def test_token_client_launch_needs_no_chatgpt_auth_or_bootstrap_proxy(self):
        self.cfg["client_auth"] = "token"
        self.cfg["providers"] = {"thirdparty": {"auth": "api_key", "base_url": "https://example.invalid/v1", "api_key_env": "UPSTREAM_SECRET"}}
        self.cfg["bootstrap_proxy"] = "http://127.0.0.1:9191"
        command, env = self.launch_codex()
        self.assertIn('model_provider="codex-gateway"', command)
        self.assertIn('model_providers.codex-gateway.base_url="http://127.0.0.1:33989/v1"', command)
        self.assertIn('model_providers.codex-gateway.env_key="CODEX_GATEWAY_TOKEN"', command)
        self.assertIn('model_providers.codex-gateway.wire_api="responses"', command)
        self.assertIn("model_providers.codex-gateway.requires_openai_auth=false", command)
        self.assertEqual(env["CODEX_GATEWAY_TOKEN"], "client-secret-value")
        self.assertNotIn("HTTPS_PROXY", env)
        self.assertNotIn("https_proxy", env)
        self.assertFalse(any(value.startswith("openai_base_url=") for value in command))
        self.assertNotIn("client-secret-value", " ".join(command))
        self.assertNotIn("admin-secret-value", " ".join(command))

    def test_first_configured_model_is_used_when_no_default_was_selected(self):
        self.cfg["default_model"] = None
        command, _ = self.launch_codex()
        self.assertIn('model="official/model"', command)

    def test_empty_catalog_does_not_start_background_service(self):
        self.cfg["default_model"] = None
        self.cfg["models"] = {}
        with mock.patch.object(lifecycle, "start") as start:
            with self.assertRaisesRegex(config.GatewayError, "No gateway models"):
                lifecycle.run_codex(self.home, [])
        start.assert_not_called()

    def test_status_checks_health_with_admin_token_and_ignores_system_proxy(self):
        with self.control_server() as server, mock.patch.dict(os.environ, {"HTTP_PROXY": "http://127.0.0.1:1", "http_proxy": "http://127.0.0.1:1", "NO_PROXY": "", "no_proxy": ""}):
            self.use_server(server)
            result = lifecycle.status(self.home)
            self.assertTrue(result["running"])
            self.assertEqual(result["pid"], self.health["pid"])
            self.assertEqual(server.seen_tokens, ["admin-secret-value"])

    def test_unrelated_service_is_never_stopped_or_replaced(self):
        with self.control_server(health={"service": "unrelated"}) as server, mock.patch.object(lifecycle.subprocess, "Popen") as spawn:
            self.use_server(server)
            for action in (lifecycle.start, lifecycle.stop, lifecycle.status):
                with self.assertRaisesRegex(config.GatewayError, "not this gateway"):
                    action(self.home)
            self.assertEqual(server.shutdown_calls, 0)
            spawn.assert_not_called()

    def test_wrong_admin_token_never_stops_service(self):
        with self.control_server(health_code=403) as server:
            self.use_server(server)
            with self.assertRaisesRegex(config.GatewayError, "ownership") as error:
                lifecycle.stop(self.home)
            self.assertNotIn("secret", str(error.exception))
            self.assertEqual(server.shutdown_calls, 0)

    def test_health_redirect_cannot_forward_admin_token(self):
        with self.control_server() as target, self.control_server(health_code=302, redirect_to=f"http://127.0.0.1:{target.server_port}/stolen") as source:
            self.use_server(source)
            with self.assertRaisesRegex(config.GatewayError, "HTTP 302") as error:
                lifecycle.status(self.home)
            self.assertNotIn("secret", str(error.exception))
            self.assertEqual(source.seen_tokens, ["admin-secret-value"])
            self.assertEqual(target.seen_tokens, [])

    def test_shutdown_redirect_cannot_forward_admin_token(self):
        with self.control_server() as target, self.control_server(shutdown_code=303, redirect_to=f"http://127.0.0.1:{target.server_port}/stolen") as source:
            self.use_server(source)
            with self.assertRaisesRegex(config.GatewayError, "HTTP 303"):
                lifecycle.stop(self.home)
            self.assertEqual(source.shutdown_calls, 1)
            self.assertEqual(target.seen_tokens, [])
            self.assertEqual(target.shutdown_calls, 0)

    def test_active_requests_prevent_stop_and_config_restart(self):
        health = {**self.health, "active_requests": 1, "fingerprint": "old"}
        with self.control_server(health=health) as server, mock.patch.object(lifecycle.subprocess, "Popen") as spawn:
            self.use_server(server)
            for action in (lifecycle.start, lifecycle.stop):
                with self.assertRaisesRegex(config.GatewayError, "active"):
                    action(self.home)
            spawn.assert_not_called()
            self.assertEqual(server.shutdown_calls, 0)

    def test_shutdown_rechecks_activity_after_health_race(self):
        with self.control_server(shutdown_code=409) as server:
            self.use_server(server)
            with self.assertRaisesRegex(config.GatewayError, "became busy"):
                lifecycle.stop(self.home)
            self.assertEqual(server.shutdown_calls, 1)

    def test_stop_uses_control_api_and_waits_for_listening_socket_to_close(self):
        with self.control_server() as server:
            self.use_server(server)
            lifecycle.start(self.home)
            self.assertEqual(lifecycle.stop(self.home), {"running": False})
            self.assertEqual(server.shutdown_calls, 1)
            self.assertFalse((self.home / "runtime" / "server.json").exists())

    def test_existing_matching_gateway_is_reused(self):
        with self.control_server() as server, mock.patch.object(lifecycle.subprocess, "Popen") as spawn:
            self.use_server(server)
            result = lifecycle.start(self.home)
            self.assertTrue(result["running"])
            spawn.assert_not_called()
            state_path = self.home / "runtime" / "server.json"
            self.assertEqual(json.loads(state_path.read_text())["port"], server.server_port)
            self.assertEqual(stat.S_IMODE(state_path.stat().st_mode), 0o600)

    def test_changed_port_finds_old_service_from_authenticated_state(self):
        with self.control_server() as server:
            state = {"host": "127.0.0.1", "port": server.server_port, "pid": 1, "fingerprint": "stale"}
            runtime = self.home / "runtime"
            runtime.mkdir()
            (runtime / "server.json").write_text(json.dumps(state))
            result = lifecycle.status(self.home)
            self.assertEqual(result["pid"], self.health["pid"])
            self.assertEqual(result["url"], f"http://127.0.0.1:{server.server_port}")

    def test_nonlocal_runtime_metadata_never_receives_admin_token(self):
        runtime = self.home / "runtime"
        runtime.mkdir()
        (runtime / "server.json").write_text(json.dumps({"host": "example.invalid", "port": 80}))
        with mock.patch.object(lifecycle, "build_opener") as network:
            with self.assertRaisesRegex(config.GatewayError, "invalid loopback"):
                lifecycle.status(self.home)
            network.assert_not_called()

    def test_stopped_service_is_idempotent_without_signalling_recorded_pid(self):
        runtime = self.home / "runtime"
        runtime.mkdir()
        (runtime / "server.json").write_text(json.dumps({"host": "127.0.0.1", "port": 33989, "pid": 1}))
        with mock.patch.object(lifecycle, "_health", return_value=None), mock.patch.object(lifecycle.os, "kill") as kill:
            self.assertEqual(lifecycle.stop(self.home), {"running": False})
            self.assertEqual(lifecycle.stop(self.home), {"running": False})
            self.assertEqual(lifecycle.status(self.home), {"running": False})
            kill.assert_not_called()

    def test_start_detaches_gateway_and_writes_private_log(self):
        process = mock.Mock(pid=12345)
        process.poll.return_value = None
        with (
            mock.patch.object(lifecycle, "_locate", return_value=(("127.0.0.1", 33989), None)),
            mock.patch.object(lifecycle, "_health", return_value=self.health),
            mock.patch.object(lifecycle.subprocess, "Popen", return_value=process) as spawn,
            mock.patch.dict(os.environ, {"PYTHONPATH": "relative-src", "CUSTOM_ENV": "keep"}, clear=True),
        ):
            result = lifecycle.start(self.home)
            self.assertNotIn("HTTPS_PROXY", os.environ)
        self.assertTrue(result["running"])
        args, kwargs = spawn.call_args
        self.assertEqual(args[0], [sys.executable, "-m", "codex_gateway", "--home", str(self.home), "serve"])
        self.assertTrue(kwargs["start_new_session"])
        self.assertEqual(kwargs["stdin"], subprocess.DEVNULL)
        self.assertEqual(kwargs["stderr"], subprocess.STDOUT)
        self.assertEqual(kwargs["env"]["CUSTOM_ENV"], "keep")
        self.assertTrue(kwargs["env"]["PYTHONPATH"].endswith(os.pathsep + "relative-src"))
        self.assertNotIn("HTTPS_PROXY", kwargs["env"])
        self.assertEqual(stat.S_IMODE((self.home / "runtime" / "server.log").stat().st_mode), 0o600)
        process.terminate.assert_not_called()

    def test_concurrent_starters_share_one_gateway(self):
        process = mock.Mock(pid=12345)
        process.poll.return_value = None
        running = threading.Event()

        def spawn_process(*args, **kwargs):
            time.sleep(0.05)
            running.set()
            return process

        with (
            mock.patch.object(lifecycle, "_health", side_effect=lambda *_: self.health if running.is_set() else None),
            mock.patch.object(lifecycle.subprocess, "Popen", side_effect=spawn_process) as spawn,
            ThreadPoolExecutor(max_workers=2) as executor,
        ):
            futures = [executor.submit(lifecycle.start, self.home) for _ in range(2)]
            results = [future.result(timeout=5) for future in futures]
        self.assertEqual(spawn.call_count, 1)
        self.assertTrue(all(result["pid"] == 12345 for result in results))

    def test_idle_gateway_restarts_after_configuration_change(self):
        events = []
        process = mock.Mock(pid=12345)
        process.poll.return_value = None

        def shutdown(*args):
            events.append("stop")

        def spawn(*args, **kwargs):
            events.append("start")
            return process

        with (
            mock.patch.object(lifecycle, "_locate", return_value=(("127.0.0.1", 33989), {**self.health, "fingerprint": "old"})),
            mock.patch.object(lifecycle, "_shutdown", side_effect=shutdown),
            mock.patch.object(lifecycle, "_health", return_value=self.health),
            mock.patch.object(lifecycle.subprocess, "Popen", side_effect=spawn),
        ):
            self.assertTrue(lifecycle.start(self.home)["running"])
        self.assertEqual(events, ["stop", "start"])

    def test_port_change_does_not_stop_old_gateway_if_new_port_is_occupied(self):
        with self.control_server(health={**self.health, "fingerprint": "old"}) as old, self.control_server(health={"service": "unrelated"}) as other:
            runtime = self.home / "runtime"
            runtime.mkdir()
            (runtime / "server.json").write_text(json.dumps({"host": "127.0.0.1", "port": old.server_port}))
            self.use_server(other)
            with self.assertRaisesRegex(config.GatewayError, "not this gateway"):
                lifecycle.start(self.home)
            self.assertEqual(old.shutdown_calls, 0)
            self.assertEqual(other.shutdown_calls, 0)

    def test_startup_failure_does_not_echo_sensitive_log_content(self):
        runtime = self.home / "runtime"
        runtime.mkdir()
        (runtime / "server.log").write_text("VERY_SECRET_API_KEY\n")
        process = mock.Mock(pid=12345)
        process.poll.return_value = 1
        with (
            mock.patch.object(lifecycle, "_locate", return_value=(("127.0.0.1", 33989), None)),
            mock.patch.object(lifecycle.subprocess, "Popen", return_value=process),
        ):
            with self.assertRaisesRegex(config.GatewayError, "exit 1") as error:
                lifecycle.start(self.home)
        self.assertIn(str(runtime / "server.log"), str(error.exception))
        self.assertNotIn("VERY_SECRET_API_KEY", str(error.exception))
        process.terminate.assert_not_called()

    def test_codex_signal_handlers_wait_for_child_and_are_restored(self):
        child = mock.Mock()
        before_int = signal.getsignal(signal.SIGINT)
        before_term = signal.getsignal(signal.SIGTERM)

        def child_wait():
            signal.getsignal(signal.SIGINT)(signal.SIGINT, None)
            child.terminate.assert_not_called()
            signal.getsignal(signal.SIGTERM)(signal.SIGTERM, None)
            child.terminate.assert_called_once_with()
            return -signal.SIGTERM

        child.wait.side_effect = child_wait
        with (
            mock.patch.object(lifecycle, "start"),
            mock.patch.object(lifecycle.shutil, "which", return_value="/existing/bin/codex"),
            mock.patch.object(lifecycle.subprocess, "Popen", return_value=child),
        ):
            self.assertEqual(lifecycle.run_codex(self.home, []), 128 + signal.SIGTERM)
        self.assertIs(signal.getsignal(signal.SIGINT), before_int)
        self.assertIs(signal.getsignal(signal.SIGTERM), before_term)
        child.wait.assert_called_once_with()

    def test_real_codex_child_survives_ctrl_c_and_receives_launcher_sigterm(self):
        fake = Path(self.temporary.name) / "fake-codex"
        ready = Path(self.temporary.name) / "ready"
        interrupted = Path(self.temporary.name) / "interrupted"
        fake.write_text(
            f"#!{sys.executable}\n"
            "import signal, sys, time\nfrom pathlib import Path\n"
            f"signal.signal(signal.SIGINT, lambda *_: Path({str(interrupted)!r}).touch())\n"
            "signal.signal(signal.SIGTERM, lambda *_: sys.exit(17))\n"
            f"Path({str(ready)!r}).touch()\n"
            "while True: time.sleep(0.05)\n"
        )
        fake.chmod(0o700)
        launcher_script = (
            "from pathlib import Path\nfrom codex_gateway import config, lifecycle\n"
            f"config.load_config = lambda _: {self.cfg!r}\n"
            "lifecycle.start = lambda _: {'running': True}\n"
            f"raise SystemExit(lifecycle.run_codex(Path({str(self.home)!r}), [], {str(fake)!r}))\n"
        )
        env = os.environ.copy()
        env["PYTHONPATH"] = str(Path(lifecycle.__file__).resolve().parents[1])
        launcher = subprocess.Popen([sys.executable, "-c", launcher_script], env=env, start_new_session=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)

        def wait_file(path):
            deadline = time.monotonic() + 5
            while not path.exists() and launcher.poll() is None and time.monotonic() < deadline:
                time.sleep(0.01)
            self.assertTrue(path.exists(), f"child did not write {path.name}")

        try:
            wait_file(ready)
            os.killpg(launcher.pid, signal.SIGINT)
            wait_file(interrupted)
            self.assertIsNone(launcher.poll(), "Ctrl-C must not detach the launcher from a still-running Codex")
            launcher.send_signal(signal.SIGTERM)
            self.assertEqual(launcher.wait(timeout=5), 17)
        finally:
            if launcher.poll() is None:
                os.killpg(launcher.pid, signal.SIGTERM)
                launcher.wait(timeout=5)
            launcher.communicate(timeout=5)


if __name__ == "__main__":
    unittest.main()
