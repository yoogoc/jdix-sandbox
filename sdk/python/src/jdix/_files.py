"""The sandbox file API.

Reads stream by default. Sandboxes routinely produce artefacts far larger than
anything worth holding in memory, so ``read_bytes`` is the convenience and
``open`` is the primitive, not the other way round.
"""

from __future__ import annotations

from contextlib import contextmanager
from typing import IO, Iterator, List, Union
from urllib.parse import quote

from ._client import _parse_time
from ._models import FileInfo


class Files:
    def __init__(self, sandbox: "Sandbox") -> None:  # noqa: F821 - cycle only for typing
        self._sbx = sandbox

    def _path_query(self, path: str) -> str:
        return f"/v1/files?path={quote(path, safe='/')}"

    @contextmanager
    def open(self, path: str) -> Iterator[IO[bytes]]:
        """Stream a file. Use this for anything that might be large."""
        response = self._sbx._request(
            "GET", self._path_query(path), stream_response=True
        )
        try:
            yield _ResponseReader(response)
        finally:
            response.close()

    def read_bytes(self, path: str) -> bytes:
        response = self._sbx._request("GET", self._path_query(path))
        try:
            return response.content
        finally:
            response.close()

    def read_text(self, path: str, encoding: str = "utf-8") -> str:
        return self.read_bytes(path).decode(encoding)

    def write(self, path: str, content: Union[bytes, str, IO[bytes]]) -> None:
        """Write a file, creating parent directories as needed."""
        if isinstance(content, str):
            content = content.encode("utf-8")
        response = self._sbx._request(
            "PUT",
            self._path_query(path),
            content=content,
            headers={"Content-Type": "application/octet-stream"},
        )
        response.close()

    def list(self, path: str) -> List[FileInfo]:
        body = self._sbx._json("GET", f"/v1/files/list?path={quote(path, safe='/')}")
        return [
            FileInfo(
                name=e.get("name", ""),
                path=e.get("path", ""),
                size=int(e.get("size", 0)),
                mode=e.get("mode", ""),
                is_dir=bool(e.get("isDir")),
                modified=_parse_time(e.get("modified")),
                symlink=bool(e.get("symlink")),
            )
            for e in body.get("entries", [])
        ]

    def remove(self, path: str, recursive: bool = False) -> None:
        query = self._path_query(path)
        if recursive:
            query += "&recursive=true"
        self._sbx._json("DELETE", query)


class _ResponseReader:
    """Adapts a streaming httpx response to a file-like object."""

    def __init__(self, response) -> None:
        self._response = response
        self._iter = response.iter_bytes()
        self._buf = b""

    def read(self, size: int = -1) -> bytes:
        if size < 0:
            chunks = [self._buf]
            self._buf = b""
            chunks.extend(self._iter)
            return b"".join(chunks)
        while len(self._buf) < size:
            try:
                self._buf += next(self._iter)
            except StopIteration:
                break
        out, self._buf = self._buf[:size], self._buf[size:]
        return out

    def __iter__(self):
        if self._buf:
            yield self._buf
            self._buf = b""
        yield from self._iter
