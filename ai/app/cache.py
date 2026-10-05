"""Disk cache for provider responses, so a re-run does not pay twice.

Every LLM completion and every embedding is stored under a SHA-256 of what
produced it: the provider, the model, the prompt and the input (see `key`).
Running the seed, the eval or a demo a second time then reads the first run's
answers back instead of calling the provider, which also makes the second
run's results identical to the first.

One JSON file per entry, `<dir>/<first two hex chars>/<key>.json`, written
with a rename so a reader never sees half a file. Only successful responses
are stored; a provider error is raised to the caller and the next run tries
again. A stored response is replayed as is. One the caller could not use at
all is taken back out (`discard`; app.llm does so when every attempt at an
answer failed validation), so that asking again asks the provider.

`AI_CACHE` picks the mode: `on` (default), `off` to bypass the cache
entirely, `refresh` to call the provider and overwrite what is stored.
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import tempfile
import threading
from collections.abc import Callable
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, Literal, cast

from app.settings import Settings

log = logging.getLogger(__name__)

Mode = Literal["on", "off", "refresh"]

DEFAULT_DIR = Path(__file__).resolve().parents[1] / ".cache"


def key(kind: str, **parts: Any) -> str:
    """The cache key for one request: a hash of its kind and every part that
    can change the answer. Parts must be JSON-serialisable."""
    payload = json.dumps({"kind": kind, **parts}, sort_keys=True, ensure_ascii=False, separators=(",", ":"))
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


class Cache:
    """Counts what it served (`hits`) and what it let through (`calls`), so a
    caller can report whether a run touched the provider at all."""

    def __init__(self, directory: Path = DEFAULT_DIR, mode: Mode = "on"):
        self.directory = directory
        self.mode: Mode = mode
        self.hits = 0
        self.calls = 0
        self._lock = threading.Lock()

    def _path(self, key: str) -> Path:
        return self.directory / key[:2] / f"{key}.json"

    def get(self, key: str) -> Any | None:
        """The stored value, or None when there is none or the mode skips reads."""
        if self.mode != "on":
            return None
        try:
            entry = cast(dict[str, Any], json.loads(self._path(key).read_text(encoding="utf-8")))
            return entry["value"]
        except FileNotFoundError:
            return None
        except (OSError, ValueError, KeyError, TypeError) as e:
            # A truncated or hand-edited entry is a miss, and the fresh answer replaces it.
            log.warning("cache: ignoring unreadable entry %s: %s", key, e)
            return None

    def put(self, key: str, value: Any, **meta: str) -> None:
        """Store a value. `meta` is written next to it for whoever opens the
        file; it plays no part in lookup."""
        if self.mode == "off":
            return
        path = self._path(key)
        entry = {"key": key, **meta, "created_at": datetime.now(UTC).isoformat(timespec="seconds"), "value": value}
        try:
            path.parent.mkdir(parents=True, exist_ok=True)
            fd, tmp = tempfile.mkstemp(dir=path.parent, suffix=".tmp")
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                json.dump(entry, f, ensure_ascii=False)
            os.replace(tmp, path)
        except OSError as e:
            # A read-only or full disk must not fail the request that already has its answer.
            log.warning("cache: could not store %s: %s", key, e)

    def discard(self, key: str) -> None:
        """Forget what is stored under `key`, if anything is."""
        try:
            self._path(key).unlink(missing_ok=True)
        except OSError as e:
            log.warning("cache: could not discard %s: %s", key, e)

    def get_or_call[T](self, key: str, call: Callable[[], T], **meta: str) -> T:
        """The stored value for `key`, or `call()` stored under it. An
        exception from `call` propagates and stores nothing."""
        cached = self.get(key)
        if cached is not None:
            with self._lock:
                self.hits += 1
            return cast(T, cached)
        with self._lock:
            self.calls += 1
        value = call()
        self.put(key, value, **meta)
        return value

    def get_or_call_many[T](
        self, keys: list[str], call: Callable[[list[str]], list[T]], *, batch: int | None = None, **meta: str
    ) -> list[T]:
        """`get_or_call` for a batch: one value per key, in order. `call`
        gets the keys with nothing stored (each once, in first-seen order,
        at most `batch` at a time) and returns their values in that order;
        it is not called when everything is stored. Each call's values are
        stored as soon as it returns, so when a later call raises, the
        exception propagates and a retry pays only for what is still missing."""
        found: dict[str, T] = {}
        missing: list[str] = []
        for key in dict.fromkeys(keys):
            cached = self.get(key)
            if cached is not None:
                found[key] = cast(T, cached)
            else:
                missing.append(key)
        with self._lock:
            self.hits += len(found)
            self.calls += len(missing)
        size = batch or len(missing) or 1
        for start in range(0, len(missing), size):
            chunk = missing[start : start + size]
            for key, value in zip(chunk, call(chunk), strict=True):
                self.put(key, value, **meta)
                found[key] = value
        return [found[key] for key in keys]


def from_settings(settings: Settings) -> Cache:
    return Cache(Path(settings.ai_cache_dir) if settings.ai_cache_dir else DEFAULT_DIR, settings.ai_cache)
