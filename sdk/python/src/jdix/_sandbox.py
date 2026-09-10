"""Client and Sandbox: the surface almost every caller touches."""

from __future__ import annotations

import base64
import json
import os
import threading
import time
from typing import TYPE_CHECKING, Any, Dict, Iterator, List, Optional, Sequence
from urllib.parse import quote, urlencode

from ._client import DEFAULT_BASE_URL, Transport, _parse_time
from ._errors import JdixError, NotFound, SandboxExpired, SandboxFailed
from ._files import Files
from ._models import NO_EXPIRY, Chunk, Mount, Process, Result, SandboxInfo, Template

if TYPE_CHECKING:  # pragma: no cover
    import httpx


class Client:
    """Entry point to a jdix-sandbox installation."""

    def __init__(
        self,
        api_key: Optional[str] = None,
        *,
        base_url: Optional[str] = None,
        timeout: float = 90.0,
        max_retries: int = 3,
        http_client: Optional["httpx.Client"] = None,
    ) -> None:
        """Create a client.

        ``http_client`` accepts a configured ``httpx.Client``, which is how you
        route through a corporate proxy, pin a CA bundle, or attach tracing. It
        is also how you opt out of the environment's proxy settings, which
        otherwise apply to loopback addresses too and make a local installation
        surprisingly hard to reach.
        """
        self._t = Transport(
            api_key=api_key or os.environ.get("JDIX_API_KEY", ""),
            base_url=base_url or os.environ.get("JDIX_BASE_URL", DEFAULT_BASE_URL),
            timeout=timeout,
            max_retries=max_retries,
            http_client=http_client,
        )

    def close(self) -> None:
        self._t.close()

    def __enter__(self) -> "Client":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()

    def create(
        self,
        template: str,
        *,
        ttl: int = 0,
        env: Optional[Dict[str, str]] = None,
        mounts: Optional[Sequence[Mount]] = None,
        metadata: Optional[Dict[str, str]] = None,
        idempotency_key: str = "",
        keep_alive: bool = False,
    ) -> "Sandbox":
        """Start a sandbox and return a handle to it.

        ``ttl`` is how long the sandbox may live, in seconds. Zero takes the
        template's default; ``NO_EXPIRY`` asks for one that never expires, which
        the template has to permit by setting no ``maxTTLSeconds``. Nothing then
        reclaims that sandbox if this process goes away, so close it yourself.

        Set ``idempotency_key`` whenever the call might be replayed — a job
        runner, a queue consumer, anything with at-least-once delivery. Without
        one, a retry starts a second sandbox and bills for both.
        """
        body: Dict[str, Any] = {"template": template}
        if ttl:
            body["ttlSeconds"] = int(ttl)
        if env:
            body["env"] = env
        if metadata:
            body["metadata"] = metadata
        if mounts:
            body["filesystem"] = {"mounts": [m.to_json() for m in mounts]}

        headers = {"Idempotency-Key": idempotency_key} if idempotency_key else None
        payload = self._t.json("POST", "/v1/sandboxes", json=body, headers=headers)

        info = _info_from_json(payload)
        if info.state == "failed":
            raise SandboxFailed(info.reason or "the sandbox could not be started",
                                code="sandbox_failed")
        sbx = Sandbox(self._t, info)
        # A sandbox that never expires has no deadline to renew, so asking for
        # both is not an error, it is just nothing to do.
        if keep_alive and ttl != NO_EXPIRY:
            sbx._start_keep_alive(ttl or 600)
        return sbx

    def get(self, sandbox_id: str) -> "Sandbox":
        """Fetch an existing sandbox, ready to execute against.

        There is no second credential to carry: the data plane takes the same
        API key as the control plane, so a sandbox fetched by id is immediately
        usable.
        """
        payload = self._t.json("GET", f"/v1/sandboxes/{quote(sandbox_id)}")
        return Sandbox(self._t, _info_from_json(payload))

    def list(self, state: str = "") -> List["Sandbox"]:
        path = "/v1/sandboxes"
        if state:
            path += "?" + urlencode({"state": state})
        payload = self._t.json("GET", path)
        return [Sandbox(self._t, _info_from_json(s)) for s in payload.get("sandboxes", [])]

    def templates(self) -> List[Template]:
        payload = self._t.json("GET", "/v1/templates")
        return [
            Template(
                name=t.get("name", ""),
                admission=t.get("admission", ""),
                admission_reason=t.get("admissionReason", ""),
                min_isolation_tier=t.get("minIsolationTier", ""),
                default_ttl_seconds=int(t.get("defaultTTLSeconds", 0)),
                max_ttl_seconds=int(t.get("maxTTLSeconds", 0)),
                has_volumes=bool(t.get("hasVolumes")),
            )
            for t in payload.get("templates", [])
        ]


def _info_from_json(payload: Dict[str, Any]) -> SandboxInfo:
    return SandboxInfo(
        id=payload.get("id", ""),
        state=payload.get("state", "pending"),
        template=payload.get("template", ""),
        isolation_tier=payload.get("isolationTier", ""),
        endpoint=payload.get("endpoint", ""),
        expires_at=_parse_time(payload.get("expiresAt")),
        cold_start=bool(payload.get("coldStart")),
        reason=payload.get("reason", ""),
    )


class Sandbox:
    """A live execution environment.

    Use it as a context manager and it deletes itself on the way out::

        with Client().create("py312-small", ttl=600) as sbx:
            print(sbx.run("python -c 'print(1+1)'").stdout)
    """

    def __init__(self, transport: Transport, info: SandboxInfo) -> None:
        self._t = transport
        self._info = info
        self._keep_alive_stop: Optional[threading.Event] = None
        self._closed = False
        self.files = Files(self)

    # ---- identity -------------------------------------------------------

    @property
    def id(self) -> str:
        return self._info.id

    @property
    def state(self) -> str:
        return self._info.state

    @property
    def endpoint(self) -> str:
        return self._info.endpoint

    @property
    def isolation_tier(self) -> str:
        return self._info.isolation_tier

    @property
    def cold_start(self) -> bool:
        return self._info.cold_start

    @property
    def expires_at(self):
        return self._info.expires_at

    def __repr__(self) -> str:  # pragma: no cover - debugging aid
        return f"<Sandbox {self._info.id} {self._info.state} template={self._info.template}>"

    # ---- data-plane plumbing -------------------------------------------

    def _request(self, method: str, path: str, **kwargs: Any):
        if not self._info.endpoint:
            raise JdixError(
                f"sandbox {self._info.id} has no endpoint yet; call wait_ready() first",
                code="not_ready",
            )
        return self._t.request(
            method, path, base=self._info.endpoint, **kwargs
        )

    def _json(self, method: str, path: str, **kwargs: Any) -> Any:
        response = self._request(method, path, **kwargs)
        try:
            return response.json() if response.content else {}
        finally:
            response.close()

    # ---- lifecycle ------------------------------------------------------

    def wait_ready(self, timeout: float = 60.0) -> "Sandbox":
        """Poll until the sandbox is running.

        Only cold starts need this: a warm-pool hit is already running by the
        time ``create`` returns.
        """
        if self._info.state == "running":
            return self
        deadline = time.monotonic() + timeout
        delay = 0.1
        while True:
            payload = self._t.json("GET", f"/v1/sandboxes/{quote(self._info.id)}")
            fresh = _info_from_json(payload)
            self._info = fresh
            if fresh.state == "running":
                return self
            if fresh.state == "failed":
                raise SandboxFailed(fresh.reason or "the sandbox failed to start",
                                    code="sandbox_failed")
            if fresh.state == "expired":
                raise SandboxExpired("the sandbox expired before it was ready")
            if time.monotonic() > deadline:
                raise JdixError(
                    f"sandbox {self._info.id} was still {fresh.state} after {timeout:g}s",
                    code="not_ready",
                )
            time.sleep(delay)
            delay = min(delay * 2, 2.0)

    def keep_alive(self, ttl: int = 300) -> None:
        payload = self._json("POST", f"/v1/keepalive?ttlSeconds={int(ttl)}")
        self._info.expires_at = _parse_time(payload.get("expiresAt"))

    def _start_keep_alive(self, ttl: int) -> None:
        stop = threading.Event()
        self._keep_alive_stop = stop
        # Renew at a third of the TTL so one missed renewal is not fatal.
        interval = max(15.0, ttl / 3)

        def loop() -> None:
            while not stop.wait(interval):
                try:
                    self.keep_alive(ttl)
                except Exception:
                    # A failed renewal is not worth crashing a background
                    # thread over; the TTL is the backstop either way.
                    return

        threading.Thread(target=loop, name=f"jdix-keepalive-{self._info.id}", daemon=True).start()

    def close(self) -> None:
        """Delete the sandbox. Safe to call more than once."""
        if self._closed:
            return
        self._closed = True
        if self._keep_alive_stop is not None:
            self._keep_alive_stop.set()
        try:
            self._t.json("DELETE", f"/v1/sandboxes/{quote(self._info.id)}")
        except NotFound:
            # Already gone, most likely because the TTL elapsed. That is the
            # outcome close() wanted.
            pass

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()

    # ---- execution ------------------------------------------------------

    def run(
        self,
        cmd: str = "",
        *,
        argv: Optional[Sequence[str]] = None,
        cwd: str = "",
        env: Optional[Dict[str, str]] = None,
        timeout: int = 0,
        max_output_bytes: int = 0,
    ) -> Result:
        """Run a command and wait for it.

        Pass ``argv`` instead of ``cmd`` when any part of the command comes from
        untrusted input: there is no shell for a quoting mistake to escape into.
        """
        body: Dict[str, Any] = {}
        if argv:
            body["argv"] = list(argv)
        elif cmd:
            body["cmd"] = cmd
        else:
            raise JdixError("run() needs either cmd or argv", code="invalid_request")
        if cwd:
            body["cwd"] = cwd
        if env:
            body["env"] = env
        if timeout:
            body["timeoutSeconds"] = int(timeout)
        if max_output_bytes:
            body["maxOutputBytes"] = int(max_output_bytes)

        payload = self._json("POST", "/v1/exec", json=body)
        return Result(
            exit_code=int(payload.get("exitCode", -1)),
            stdout=payload.get("stdout", ""),
            stderr=payload.get("stderr", ""),
            duration_ms=int(payload.get("durationMs", 0)),
            truncated=bool(payload.get("truncated")),
            timed_out=bool(payload.get("timedOut")),
        )

    def run_stream(self, cmd: str, *, timeout: int = 0) -> Iterator[Chunk]:
        """Run a command and yield its output as it arrives.

        The generator ends when the command exits; its status is available
        afterwards as ``.exit_code`` on the generator object.
        """
        from ._stream import stream_exec

        return stream_exec(self, cmd, timeout=timeout)

    def processes(self) -> List[Process]:
        payload = self._json("GET", "/v1/processes")
        return [
            Process(
                pid=int(p.get("pid", 0)),
                cmd=p.get("cmd", ""),
                started=p.get("started", ""),
                running=bool(p.get("running")),
            )
            for p in payload.get("processes", [])
        ]

    def signal(self, pid: int, sig: str = "SIGTERM") -> None:
        self._json("DELETE", f"/v1/processes/{int(pid)}?signal={quote(sig)}")

    def expose(self, port: int) -> str:
        """Return the URL a port inside the sandbox is reachable at.

        The URL comes from the server and is deliberately not derived here. How
        a sandbox is published — a subdomain per sandbox, or a path under one
        hostname — is the gateway's configuration, and a client that guessed
        would be right only for whichever scheme it was written against.

        Any port the sandbox is listening on is already reachable to a caller
        holding the tenant's API key. This reports where; it does not grant
        anything.
        """
        payload = self._json("POST", "/v1/ports", json={"port": int(port), "public": True})
        url = payload.get("url")
        if not url:
            raise JdixError(
                "the gateway did not say where this port is published", code="no_url"
            )
        return str(url)
