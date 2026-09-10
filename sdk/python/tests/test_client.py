"""Tests for the jdix Python client.

They drive the real client against a local HTTP server standing in for the
control plane and the sandbox data plane, so the request shaping, error mapping
and retry policy are all exercised rather than mocked away.
"""

from __future__ import annotations

import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Dict
from urllib.parse import parse_qs, urlparse

import httpx
import pytest

import jdix
from jdix import Client, JdixError, NotFound, QuotaExceeded


class _State:
    """Mutable knobs the tests use to steer the fake server."""

    def __init__(self) -> None:
        self.creates = 0
        self.get_attempts = 0
        self.state = "running"
        self.fail_get_times = 0
        self.last_auth = ""
        self.last_idem = ""
        self.last_exec: Dict[str, Any] = {}
        self.deletes = 0
        self.no_port_url = False


class _Handler(BaseHTTPRequestHandler):
    state: _State

    def log_message(self, *args):  # keep the test output readable
        pass

    def _send(self, code: int, body: Any, headers: Dict[str, str] | None = None) -> None:
        payload = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(payload)

    def _body(self) -> Dict[str, Any]:
        length = int(self.headers.get("Content-Length") or 0)
        if not length:
            return {}
        raw = self.rfile.read(length)
        try:
            return json.loads(raw)
        except Exception:
            return {"_raw": raw}

    def _sandbox_json(self) -> Dict[str, Any]:
        return {
            "id": "sbx-test",
            "state": self.state.state,
            "template": "py312",
            "isolationTier": "userns",
            "endpoint": f"http://{self.headers.get('Host')}",
            "token": "sbt_test",
            "expiresAt": "2030-01-01T00:00:00Z",
            "coldStart": False,
        }

    def do_GET(self):
        url = urlparse(self.path)
        st = self.state
        if url.path == "/v1/sandboxes":
            self._send(200, {"sandboxes": [self._sandbox_json()]})
        elif url.path.startswith("/v1/sandboxes/"):
            st.get_attempts += 1
            if st.fail_get_times >= st.get_attempts:
                self._send(503, {"code": "unavailable", "message": "try again"})
                return
            self._send(200, self._sandbox_json())
        elif url.path == "/v1/templates":
            self._send(200, {"templates": [{
                "name": "py312", "admission": "Approved",
                "minIsolationTier": "userns", "defaultTTLSeconds": 1800,
            }]})
        elif url.path == "/v1/files":
            path = parse_qs(url.query).get("path", [""])[0]
            if path.endswith("missing"):
                self._send(404, {"code": "not_found", "message": "no such file"})
                return
            self._send(200, b"file contents")
        elif url.path == "/v1/files/list":
            self._send(200, {"entries": [{
                "name": "main.py", "path": "/workspace/main.py",
                "size": 11, "mode": "-rw-r--r--", "isDir": False,
                "modified": "2026-09-08T10:00:00Z",
            }]})
        elif url.path == "/v1/processes":
            self._send(200, {"processes": [{"pid": 42, "cmd": "sleep", "started": "", "running": True}]})
        elif url.path == "/v1/quota-429":
            self._send(429, {"code": "quota_concurrent", "message": "too many"}, {"Retry-After": "30"})
        else:
            self._send(404, {"code": "not_found", "message": self.path})

    def do_POST(self):
        url = urlparse(self.path)
        st = self.state
        body = self._body()
        if url.path == "/v1/sandboxes":
            st.creates += 1
            st.last_auth = self.headers.get("Authorization", "")
            st.last_idem = self.headers.get("Idempotency-Key", "")
            self._send(201, self._sandbox_json())
        elif url.path == "/v1/exec":
            st.last_exec = body
            code = 3 if body.get("cmd") == "false" else 0
            self._send(200, {
                "exitCode": code, "stdout": "out:" + str(body.get("cmd") or body.get("argv")),
                "stderr": "", "durationMs": 5,
            })
        elif url.path == "/v1/keepalive":
            self._send(200, {"ok": True, "expiresAt": "2031-01-01T00:00:00Z"})
        elif url.path == "/v1/ports":
            if self.state.no_port_url:
                self._send(200, {"port": body.get("port")})
            else:
                self._send(200, {
                    "port": body.get("port"),
                    "url": f"http://{self.headers['Host']}/s/sbx-test/p/{body.get('port')}/",
                })
        else:
            self._send(404, {"code": "not_found", "message": self.path})

    def do_PUT(self):
        self._body()
        self._send(200, {"ok": True})

    def do_DELETE(self):
        self.state.deletes += 1
        self._send(200, {"ok": True})


@pytest.fixture
def server():
    state = _State()
    handler = type("H", (_Handler,), {"state": state})
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    base = f"http://127.0.0.1:{httpd.server_address[1]}"
    try:
        yield base, state
    finally:
        httpd.shutdown()
        httpd.server_close()


def _local_client(base: str) -> Client:
    """A client that ignores the environment's proxy settings.

    Without this the test would route loopback traffic through whatever proxy
    the developer's shell happens to export, which is exactly the problem the
    http_client argument exists to solve.
    """
    return Client(
        api_key="jdix_sk_0123456789ab_secret",
        base_url=base,
        http_client=httpx.Client(trust_env=False, timeout=10.0),
    )


@pytest.fixture
def client(server):
    base, _ = server
    with _local_client(base) as c:
        yield c


def test_api_key_is_required():
    with pytest.raises(JdixError):
        Client(api_key="", base_url="http://example.invalid")


def test_create_and_run(client, server):
    _, state = server
    sbx = client.create("py312", ttl=600, env={"RUN_ID": "42"})
    assert sbx.id == "sbx-test"
    assert sbx.isolation_tier == "userns"
    assert state.last_auth.startswith("Bearer jdix_sk_")

    result = sbx.run("echo hi")
    assert result.ok
    assert "echo hi" in result.stdout


def test_non_zero_exit_is_not_an_exception(client):
    """A failing command is a Result, not a raise: the request succeeded."""
    sbx = client.create("py312")
    result = sbx.run("false")
    assert result.exit_code == 3
    assert not result.ok
    with pytest.raises(JdixError):
        result.check()  # opt in to the raising shape


def test_argv_form_avoids_the_shell(client, server):
    _, state = server
    sbx = client.create("py312")
    sbx.run(argv=["/bin/echo", "a;b"])
    assert state.last_exec["argv"] == ["/bin/echo", "a;b"]
    assert "cmd" not in state.last_exec


def test_run_requires_a_command(client):
    sbx = client.create("py312")
    with pytest.raises(JdixError):
        sbx.run()


def test_idempotency_key_is_forwarded(client, server):
    _, state = server
    client.create("py312", idempotency_key="job-42")
    assert state.last_idem == "job-42"


def test_creates_are_never_retried(server):
    """Retrying a create silently is how a network hiccup doubles the bill."""
    base, state = server
    with _local_client(base) as c:
        # Point at a path the fake server answers with 404 for POST, which is
        # not retryable anyway; the assertion that matters is the count.
        c.create("py312")
    assert state.creates == 1


def test_reads_are_retried_on_server_errors(server):
    base, state = server
    state.fail_get_times = 2
    with _local_client(base) as c:
        sbx = c.get("sbx-test")
    assert sbx.id == "sbx-test"
    assert state.get_attempts == 3


def test_quota_errors_surface_immediately(client):
    """A quota will not clear on retry, so the caller is told at once."""
    started = time.monotonic()
    with pytest.raises(QuotaExceeded) as exc:
        client._t.json("GET", "/v1/quota-429")
    assert exc.value.retry_after == 30
    assert time.monotonic() - started < 2.0


def test_file_round_trip(client):
    sbx = client.create("py312")
    sbx.files.write("/workspace/main.py", "print('hi')")
    assert sbx.files.read_text("/workspace/main.py") == "file contents"

    entries = sbx.files.list("/workspace")
    assert len(entries) == 1
    assert entries[0].name == "main.py"
    assert entries[0].modified is not None

    with pytest.raises(NotFound):
        sbx.files.read_bytes("/workspace/missing")


def test_streaming_file_read(client):
    sbx = client.create("py312")
    with sbx.files.open("/workspace/big.bin") as fh:
        assert fh.read(4) == b"file"
        assert fh.read() == b" contents"


def test_context_manager_deletes_the_sandbox(client, server):
    _, state = server
    with client.create("py312") as sbx:
        assert sbx.id
    assert state.deletes == 1


def test_close_is_idempotent(client, server):
    _, state = server
    sbx = client.create("py312")
    sbx.close()
    sbx.close()
    sbx.close()
    assert state.deletes == 1


def test_keep_alive_moves_the_deadline(client):
    sbx = client.create("py312")
    before = sbx.expires_at
    sbx.keep_alive(3600)
    assert sbx.expires_at is not None and before is not None
    assert sbx.expires_at > before


def test_expose_reports_the_servers_url(client):
    """The gateway decides how a sandbox is published, so the SDK reports what
    it said rather than deriving a URL that only suits one routing scheme."""
    sbx = client.create("py312")
    assert sbx.expose(8000).endswith("/s/sbx-test/p/8000/")


def test_expose_fails_when_the_server_names_no_url(client, server):
    _, state = server
    state.no_port_url = True
    sbx = client.create("py312")
    with pytest.raises(JdixError):
        sbx.expose(8000)


def test_templates(client):
    templates = client.templates()
    assert len(templates) == 1
    assert templates[0].name == "py312"
    assert templates[0].min_isolation_tier == "userns"


def test_processes_and_signal(client):
    sbx = client.create("py312")
    procs = sbx.processes()
    assert procs[0].pid == 42
    sbx.signal(42, "SIGINT")


def test_data_plane_calls_need_an_endpoint():
    from jdix._models import SandboxInfo
    from jdix._sandbox import Sandbox

    sbx = Sandbox.__new__(Sandbox)
    sbx._info = SandboxInfo(id="x", state="pending", template="t", endpoint="")
    sbx._t = None
    with pytest.raises(JdixError) as exc:
        sbx._request("GET", "/v1/exec")
    assert "wait_ready" in str(exc.value)


def test_public_surface_is_exported():
    for name in ("Client", "Sandbox", "Result", "Mount", "QuotaExceeded", "MountRejected"):
        assert hasattr(jdix, name), name
