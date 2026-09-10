"""Streaming execution over the shared WebSocket frame protocol."""

from __future__ import annotations

import base64
import json
from typing import Iterator, Optional
from urllib.parse import urlencode

from ._errors import JdixError
from ._models import Chunk


class ExecStream:
    """Yields output chunks and records the exit status.

    ``exit_code`` is None until the command finishes, which is the honest answer
    while it is still running.
    """

    def __init__(self, sandbox, path: str, params: dict) -> None:
        self._sbx = sandbox
        self._path = path
        self._params = params
        self.exit_code: Optional[int] = None
        self.duration_ms: int = 0

    def _ws_url(self) -> str:
        endpoint = self._sbx.endpoint
        if not endpoint:
            raise JdixError("this sandbox has no endpoint yet", code="not_ready")
        url = endpoint.replace("https://", "wss://").replace("http://", "ws://")
        query = urlencode({k: v for k, v in self._params.items() if v})
        return f"{url.rstrip('/')}{self._path}" + (f"?{query}" if query else "")

    def __iter__(self) -> Iterator[Chunk]:
        # Imported lazily so that installing the package without websockets
        # still allows the synchronous API to work.
        from websockets.sync.client import connect

        headers = {"Authorization": f"Bearer {self._sbx._t.api_key}"}
        with connect(self._ws_url(), additional_headers=headers, max_size=32 * 1024 * 1024) as ws:
            for raw in ws:
                frame = json.loads(raw)
                kind = frame.get("t")
                if kind in ("stdout", "stderr"):
                    yield Chunk(stream=kind, data=base64.b64decode(frame.get("d", "")))
                elif kind == "exit":
                    self.exit_code = int(frame.get("code", -1))
                    self.duration_ms = int(frame.get("durationMs", 0))
                    return
                elif kind == "error":
                    raise JdixError(frame.get("msg", "stream error"), code=frame.get("errCode", ""))
                elif kind == "ping":
                    # The server pings at the application level because load
                    # balancers routinely drop WebSocket control frames.
                    ws.send(json.dumps({"t": "pong"}))


def stream_exec(sandbox, cmd: str, *, timeout: int = 0) -> ExecStream:
    params = {"cmd": cmd}
    if timeout:
        params["timeoutSeconds"] = str(int(timeout))
    return ExecStream(sandbox, "/v1/exec/stream", params)
