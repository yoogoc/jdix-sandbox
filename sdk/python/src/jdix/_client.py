"""HTTP plumbing shared by the sync client and the sandbox handle."""

from __future__ import annotations

import os
import random
import time
from datetime import datetime, timezone
from typing import Any, Dict, Optional

import httpx

from ._errors import JdixError, raise_for_response

DEFAULT_BASE_URL = "https://api.jdix.example.com"
USER_AGENT = "jdix-python/0.1"

# Only reads are retried, and only on failures a repeat might fix. Creates are
# never retried: the SDK does not set an idempotency key on the caller's behalf,
# and a silent retry is how a network hiccup turns into two sandboxes.
_RETRY_STATUSES = frozenset({500, 502, 503, 504})
_RETRY_METHODS = frozenset({"GET", "HEAD"})


def _parse_time(value: Any) -> Optional[datetime]:
    if not value or not isinstance(value, str):
        return None
    text = value.replace("Z", "+00:00")
    try:
        parsed = datetime.fromisoformat(text)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed


class Transport:
    """A thin retrying wrapper around httpx."""

    def __init__(
        self,
        *,
        api_key: str,
        base_url: str = DEFAULT_BASE_URL,
        timeout: float = 90.0,
        max_retries: int = 3,
        http_client: Optional[httpx.Client] = None,
    ) -> None:
        if not api_key:
            raise JdixError(
                "an API key is required: pass api_key= or set JDIX_API_KEY",
                code="missing_api_key",
            )
        self.api_key = api_key
        self.base_url = base_url.rstrip("/")
        self.max_retries = max_retries
        self._owns_client = http_client is None
        self._client = http_client or httpx.Client(timeout=timeout, follow_redirects=False)

    def close(self) -> None:
        if self._owns_client:
            self._client.close()

    def request(
        self,
        method: str,
        path: str,
        *,
        json: Any = None,
        content: Any = None,
        headers: Optional[Dict[str, str]] = None,
        base: Optional[str] = None,
        token: Optional[str] = None,
        stream_response: bool = False,
    ) -> httpx.Response:
        url = (base or self.base_url).rstrip("/") + path
        merged = {
            "Authorization": f"Bearer {token or self.api_key}",
            "User-Agent": USER_AGENT,
        }
        if headers:
            merged.update(headers)

        last_exc: Optional[Exception] = None
        for attempt in range(self.max_retries + 1):
            if attempt:
                # Full jitter, so a fleet of clients recovering from one blip
                # does not synchronise into a second one.
                time.sleep(random.uniform(0, min(2.0, 0.1 * (2 ** attempt))))
            try:
                request = self._client.build_request(
                    method, url, json=json, content=content, headers=merged
                )
                response = self._client.send(request, stream=stream_response)
            except httpx.HTTPError as exc:
                last_exc = exc
                if method not in _RETRY_METHODS or attempt == self.max_retries:
                    raise JdixError(f"request failed: {exc}", code="transport_error") from exc
                continue

            if response.status_code < 400:
                return response

            if (
                method in _RETRY_METHODS
                and response.status_code in _RETRY_STATUSES
                and attempt < self.max_retries
            ):
                response.close()
                continue

            self._raise(response)

        raise JdixError(f"request failed: {last_exc}", code="transport_error")

    @staticmethod
    def _raise(response: httpx.Response) -> None:
        retry_after: Optional[float] = None
        raw = response.headers.get("Retry-After")
        if raw:
            try:
                retry_after = float(raw)
            except ValueError:
                retry_after = None
        try:
            body = response.json()
            if not isinstance(body, dict):
                body = {"message": str(body)}
        except Exception:
            body = {"message": response.text[:500]}
        finally:
            response.close()
        raise_for_response(response.status_code, body, retry_after)

    def json(self, method: str, path: str, **kwargs: Any) -> Any:
        response = self.request(method, path, **kwargs)
        try:
            if not response.content:
                return {}
            return response.json()
        finally:
            response.close()
