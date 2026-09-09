"""Exceptions raised by the jdix client.

The hierarchy exists so callers can catch the case they can actually handle.
``except JdixError`` catches everything; ``except QuotaExceeded`` catches the
one thing worth backing off on.
"""

from __future__ import annotations

from typing import Optional


class JdixError(Exception):
    """Base class for every error this package raises."""

    def __init__(self, message: str, *, code: str = "", status: int = 0) -> None:
        super().__init__(message)
        self.message = message
        self.code = code
        self.status = status

    def __str__(self) -> str:  # pragma: no cover - trivial
        if self.code:
            return f"{self.code} ({self.status}): {self.message}"
        return self.message


class AuthenticationError(JdixError):
    """The API key is missing, wrong, revoked or expired."""


class PermissionDenied(JdixError):
    """The key is valid but not allowed to do this."""


class NotFound(JdixError):
    """No such sandbox, template or file."""


class QuotaExceeded(JdixError):
    """A tenant limit was hit.

    ``retry_after`` carries the server's hint in seconds when it gave one. The
    client does not sleep on your behalf: a quota does not clear in the next few
    hundred milliseconds, and turning a clear error into a long hang helps
    nobody.
    """

    def __init__(self, message: str, *, code: str = "", status: int = 0,
                 retry_after: Optional[float] = None) -> None:
        super().__init__(message, code=code, status=status)
        self.retry_after = retry_after


class SandboxExpired(JdixError):
    """The sandbox's TTL elapsed and it no longer exists."""


class SandboxFailed(JdixError):
    """The sandbox could not be started. ``message`` says why."""


class MountRejected(JdixError):
    """The requested filesystem layout was refused.

    The message names the offending field, because a rejected mount is almost
    always a fixable mistake in the request rather than a platform failure.
    """


class SandboxTimeout(JdixError):
    """A command exceeded its deadline and was terminated."""


def raise_for_response(status: int, body: dict, retry_after: Optional[float] = None) -> None:
    """Translate an error body into the most specific exception available."""
    code = str(body.get("code", ""))
    message = str(body.get("message", "")) or f"request failed with status {status}"

    if status == 401:
        raise AuthenticationError(message, code=code, status=status)
    if status == 403:
        raise PermissionDenied(message, code=code, status=status)
    if status == 404:
        raise NotFound(message, code=code, status=status)
    if status == 429:
        raise QuotaExceeded(message, code=code, status=status, retry_after=retry_after)
    if status == 410:
        raise SandboxExpired(message, code=code, status=status)
    if code in ("bind_failed", "path_forbidden", "path_escape", "read_only"):
        raise MountRejected(message, code=code, status=status)
    raise JdixError(message, code=code, status=status)
