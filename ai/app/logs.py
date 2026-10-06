"""Structured logs, and the request id that ties them to the API's.

Every log line of the service is one JSON object: `time`, `level`, `service`,
`logger`, `msg`, the `request_id` of the request it was written under, and
whatever the call passed as `extra`. The Go API writes the same keys
(api/internal/reqlog), so one id finds a request in both services' logs.

The id comes from the caller's `X-Request-ID` header (the API sends the id of
the HTTP request or job attempt it is working on) or is made up here for a
request that has none, and goes back in the response's header. The middleware
also writes the access line: method, path, status, how long it took and, when
the request asked the chat model for anything, what that spent (app/usage.py).
"""

from __future__ import annotations

import json
import logging
import os
import re
import secrets
import time
from contextvars import ContextVar
from datetime import UTC, datetime
from typing import Any

from starlette.types import ASGIApp, Message, Receive, Scope, Send

from app import usage

SERVICE = "ai"
HEADER = "X-Request-ID"
# What an id may look like when it comes from outside: it is written to logs and echoed in a header.
_VALID_ID = re.compile(r"[A-Za-z0-9._:-]{1,64}")
# Requests logged at DEBUG: the health probe runs every few seconds.
QUIET_PATHS = frozenset({"/health"})

_request_id: ContextVar[str] = ContextVar("request_id", default="")

log = logging.getLogger(__name__)

# The attributes every LogRecord has; anything else on one was passed as `extra`.
_RECORD_FIELDS = frozenset(vars(logging.LogRecord("", 0, "", 0, "", None, None))) | {"message", "asctime", "taskName"}
# The level names the API's logs use.
_LEVELS = {"WARNING": "WARN", "CRITICAL": "ERROR"}


def request_id() -> str:
    """The id of the request being handled, or "" outside one."""
    return _request_id.get()


def new_id() -> str:
    return secrets.token_hex(8)


class JSONFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        entry: dict[str, Any] = {
            "time": datetime.fromtimestamp(record.created, UTC).isoformat(timespec="milliseconds"),
            "level": _LEVELS.get(record.levelname, record.levelname),
            "service": SERVICE,
            "logger": record.name,
            "msg": record.getMessage(),
        }
        if rid := _request_id.get():
            entry["request_id"] = rid
        entry.update({k: v for k, v in vars(record).items() if k not in _RECORD_FIELDS and k not in entry})
        if record.exc_info:
            entry["exception"] = self.formatException(record.exc_info)
        # Compact, as the API writes its lines, so one pattern finds a field in both.
        return json.dumps(entry, ensure_ascii=False, default=str, separators=(",", ":"))


class _JSONHandler(logging.StreamHandler):  # pyright: ignore[reportMissingTypeArgument]
    """The handler `configure` installs, as a type of its own so it is installed once."""


def configure() -> None:
    """Send every log line of the process to stderr as JSON, uvicorn's included.

    `LOG_LEVEL` sets the level (default INFO; DEBUG adds the health probes).
    uvicorn's access log is turned off: the middleware's line replaces it."""
    root = logging.getLogger()
    if not any(isinstance(h, _JSONHandler) for h in root.handlers):
        handler = _JSONHandler()
        handler.setFormatter(JSONFormatter())
        root.addHandler(handler)
    # The same names as the API takes (debug, info, warn, error), in any case.
    level = os.environ.get("LOG_LEVEL", "").strip().upper()
    root.setLevel({"DEBUG": logging.DEBUG, "WARN": logging.WARNING, "ERROR": logging.ERROR}.get(level, logging.INFO))
    for name in ("uvicorn", "uvicorn.error", "uvicorn.access"):
        logger = logging.getLogger(name)
        logger.handlers.clear()
        logger.propagate = True
    logging.getLogger("uvicorn.access").disabled = True


class RequestContext:
    """ASGI middleware: gives the request its id and its usage meter, echoes the
    id in the response and writes the access line.

    Plain ASGI rather than Starlette's BaseHTTPMiddleware, so the endpoint runs
    in this task's context and sees both (a sync endpoint's worker thread gets
    a copy of the context, which holds the same meter)."""

    def __init__(self, app: ASGIApp):
        self.app = app

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return
        given = next((v.decode("latin-1") for k, v in scope["headers"] if k == HEADER.lower().encode()), "")
        rid = given if _VALID_ID.fullmatch(given) else new_id()
        token = _request_id.set(rid)
        meter = usage.start()
        status = 500  # what the client gets when the app raises instead of answering
        started = time.perf_counter()

        async def send_with_id(message: Message) -> None:
            nonlocal status
            if message["type"] == "http.response.start":
                status = message["status"]
                message["headers"] = [*message.get("headers", []), (HEADER.lower().encode(), rid.encode())]
            await send(message)

        try:
            await self.app(scope, receive, send_with_id)
        finally:
            fields: dict[str, Any] = {
                "method": scope["method"],
                "path": scope["path"],
                "status": status,
                "duration_ms": round((time.perf_counter() - started) * 1000),
            }
            if meter.completions:
                fields |= {
                    "llm_calls": meter.llm_calls,
                    "cache_hits": meter.cache_hits,
                    "input_tokens": meter.input_tokens,
                    "output_tokens": meter.output_tokens,
                }
            log.log(logging.DEBUG if scope["path"] in QUIET_PATHS else logging.INFO, "request", extra=fields)
            _request_id.reset(token)
