"""Value types returned by the client."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime
from typing import List, Optional


@dataclass
class Result:
    """The outcome of a finished command.

    A non-zero ``exit_code`` is not an exception. The request succeeded and the
    command failed, and those are different things — raising would force every
    caller into a try/except just to read an exit status.
    """

    exit_code: int
    stdout: str = ""
    stderr: str = ""
    duration_ms: int = 0
    truncated: bool = False
    timed_out: bool = False

    @property
    def ok(self) -> bool:
        return self.exit_code == 0

    def check(self) -> "Result":
        """Raise if the command failed, for callers who want that shape."""
        if not self.ok:
            from ._errors import JdixError

            raise JdixError(
                f"command exited {self.exit_code}: {(self.stderr or self.stdout)[:400]}",
                code="command_failed",
            )
        return self


@dataclass
class Chunk:
    """One piece of streamed output."""

    stream: str  # "stdout" or "stderr"
    data: bytes

    @property
    def text(self) -> str:
        return self.data.decode("utf-8", errors="replace")


@dataclass
class FileInfo:
    name: str
    path: str
    size: int
    mode: str
    is_dir: bool
    modified: Optional[datetime] = None
    symlink: bool = False


@dataclass
class Process:
    pid: int
    cmd: str
    started: str
    running: bool


@dataclass
class Template:
    name: str
    admission: str
    min_isolation_tier: str
    default_ttl_seconds: int = 0
    max_ttl_seconds: int = 0
    has_volumes: bool = False
    admission_reason: str = ""


@dataclass
class Mount:
    """Binds part of a template-declared volume into the sandbox."""

    path: str
    volume: str
    sub_path: str = ""
    read_only: bool = False

    def to_json(self) -> dict:
        return {
            "path": self.path,
            "source": {"volume": self.volume, "subPath": self.sub_path},
            "readOnly": self.read_only,
        }


@dataclass
class SandboxInfo:
    id: str
    state: str
    template: str
    isolation_tier: str = ""
    endpoint: str = ""
    expires_at: Optional[datetime] = None
    cold_start: bool = False
    reason: str = ""
    mounts: List[Mount] = field(default_factory=list)
