"""The response cache: keys, modes, and the wrappers that put it in front of
the LLM providers, the embedding call and the seed generator."""

from __future__ import annotations

from datetime import date
from pathlib import Path
from types import SimpleNamespace
from typing import Any, cast

import anthropic
import pytest

from app import cache as cachemod
from app import embeddings, llm
from app.cache import Cache
from app.llm import CachingProvider, ProviderError, ScriptedProvider
from app.seedgen import generate, plan
from app.settings import Settings


def test_key_depends_on_every_part_and_not_on_their_order():
    base = cachemod.key("llm", model="m", system="s", user="u")
    assert base == cachemod.key("llm", user="u", system="s", model="m")
    assert len(base) == 64
    assert base != cachemod.key("llm", model="m2", system="s", user="u")
    assert base != cachemod.key("llm", model="m", system="s2", user="u")
    assert base != cachemod.key("llm", model="m", system="s", user="u2")
    assert base != cachemod.key("embedding", model="m", system="s", user="u")


def test_second_lookup_is_served_from_disk(tmp_path: Path):
    calls: list[int] = []

    def answer() -> str:
        calls.append(1)
        return "value"

    first = Cache(tmp_path)
    assert first.get_or_call("k" * 64, answer) == "value"
    assert (first.calls, first.hits) == (1, 0)
    # A new object over the same directory: what a second process sees.
    second = Cache(tmp_path)
    assert second.get_or_call("k" * 64, answer) == "value"
    assert (second.calls, second.hits) == (0, 1)
    assert len(calls) == 1


def test_off_bypasses_the_cache_in_both_directions(tmp_path: Path):
    Cache(tmp_path).put("a" * 64, "stored")
    off = Cache(tmp_path, mode="off")
    assert off.get_or_call("a" * 64, lambda: "fresh") == "fresh"
    assert off.get_or_call("b" * 64, lambda: "fresh") == "fresh"
    assert (off.calls, off.hits) == (2, 0)
    on = Cache(tmp_path)
    assert on.get("a" * 64) == "stored"
    assert on.get("b" * 64) is None


def test_refresh_calls_the_provider_and_overwrites(tmp_path: Path):
    Cache(tmp_path).put("a" * 64, "old")
    refresh = Cache(tmp_path, mode="refresh")
    assert refresh.get_or_call("a" * 64, lambda: "new") == "new"
    assert refresh.calls == 1
    assert Cache(tmp_path).get("a" * 64) == "new"


def test_failed_call_stores_nothing(tmp_path: Path):
    cache = Cache(tmp_path)

    def boom() -> str:
        raise ProviderError("rate limited")

    with pytest.raises(ProviderError):
        cache.get_or_call("a" * 64, boom)
    assert cache.get("a" * 64) is None


def test_unreadable_entry_is_a_miss_and_is_replaced(tmp_path: Path):
    cache = Cache(tmp_path)
    cache.put("a" * 64, "good")
    path = next(tmp_path.rglob("*.json"))
    path.write_text("{not json")
    assert cache.get_or_call("a" * 64, lambda: "again") == "again"
    assert Cache(tmp_path).get("a" * 64) == "again"


def test_from_settings_uses_the_configured_directory_and_mode(tmp_path: Path):
    cache = cachemod.from_settings(Settings(ai_cache="off", ai_cache_dir=str(tmp_path)))
    assert (cache.directory, cache.mode) == (tmp_path, "off")
    assert cachemod.from_settings(Settings(ai_cache_dir="")).directory == cachemod.DEFAULT_DIR


# --- LLM providers -----------------------------------------------------------------


def test_caching_provider_replays_a_completion_without_calling_the_provider(tmp_path: Path):
    inner = ScriptedProvider(["one"])
    provider = CachingProvider(inner, Cache(tmp_path))
    assert provider.name == "scripted"
    assert provider.complete("sys", "user", {"title": "T"}) == "one"
    assert provider.complete("sys", "user", {"title": "T"}) == "one"
    assert len(inner.calls) == 1
    # A different prompt, schema or model is a different entry.
    inner.queue("two", "three", "four")
    assert provider.complete("sys", "other user", {"title": "T"}) == "two"
    assert provider.complete("sys", "user", {"title": "U"}) == "three"
    inner.model = "scripted-2"
    assert CachingProvider(inner, Cache(tmp_path)).complete("sys", "user", {"title": "T"}) == "four"


def test_caching_provider_does_not_store_provider_errors(tmp_path: Path):
    inner = ScriptedProvider([ProviderError("rate limited"), "ok"])
    provider = CachingProvider(inner, Cache(tmp_path))
    with pytest.raises(ProviderError):
        provider.complete("s", "u", {})
    assert provider.complete("s", "u", {}) == "ok"


def test_build_provider_puts_the_configured_cache_in_front(tmp_path: Path):
    provider = llm.build_provider(Settings(llm_provider="fake", ai_cache="off", ai_cache_dir=str(tmp_path)))
    assert (provider.name, provider.cache.mode, provider.cache.directory) == ("fake", "off", tmp_path)
    assert not any(tmp_path.iterdir())  # constructing a provider writes nothing


# --- embeddings --------------------------------------------------------------------


def test_provider_embeddings_are_cached_on_model_and_text(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    calls: list[tuple[str, list[str]]] = []

    def fake_openai(texts: list[str], settings: Settings) -> list[list[float]]:
        calls.append((settings.embedding_model, texts))
        return [[0.25, float(len(calls))] for _ in texts]

    monkeypatch.setattr(embeddings, "_embed_openai", fake_openai)
    settings = Settings(embedding_provider="openai", embedding_dim=2, ai_cache_dir=str(tmp_path))
    first = embeddings.embed("hello", settings)
    assert embeddings.embed("hello", settings) == first
    assert len(calls) == 1
    embeddings.embed("other", settings)
    embeddings.embed("hello", settings.model_copy(update={"embedding_model": "text-embedding-3-large"}))
    assert len(calls) == 3
    # The bypass flag reaches the provider even for a stored text.
    assert embeddings.embed("hello", settings.model_copy(update={"ai_cache": "off"})) != first
    assert len(calls) == 4


def test_local_embeddings_never_touch_the_cache(tmp_path: Path):
    settings = Settings(embedding_provider="local", embedding_dim=8, ai_cache_dir=str(tmp_path))
    embeddings.embed("hello", settings)
    assert not any(tmp_path.iterdir())


# --- seed generation ---------------------------------------------------------------


class _StubMessages:
    def __init__(self) -> None:
        self.requests = 0

    def create(self, **_: Any) -> Any:
        self.requests += 1
        return SimpleNamespace(
            usage=SimpleNamespace(input_tokens=10, output_tokens=20),
            stop_reason="end_turn",
            content=[SimpleNamespace(type="text", text='{"candidates": []}')],
        )


def test_seed_generation_does_not_pay_twice_for_the_same_batch(tmp_path: Path):
    messages = _StubMessages()
    client = cast(anthropic.Anthropic, SimpleNamespace(beta=SimpleNamespace(messages=messages)))
    slots = plan.build_plan(count=5)

    def call(mode: cachemod.Mode = "on", feedback: dict[int, str] | None = None) -> generate.Generator:
        gen = generate.Generator(client=client, today=date(2026, 9, 30), cache=Cache(tmp_path, mode))
        gen._call(slots, feedback or {})  # pyright: ignore[reportPrivateUsage]
        return gen

    assert call().usage_out == 20
    rerun = call()
    assert messages.requests == 1
    assert (rerun.cache.hits, rerun.usage_out) == (1, 0)
    # A retry with feedback is a different prompt; --no-cache always calls.
    call(feedback={slots[0].index: "fix the dates"})
    assert messages.requests == 2
    call(mode="off")
    assert messages.requests == 3


def test_an_answer_that_never_validated_is_not_replayed_to_a_retry(tmp_path: Path):
    """The caller reports invalid output as retryable. A retry that got the
    same rejected answers back from the cache could never succeed."""
    from pydantic import BaseModel

    class Answer(BaseModel):
        count: int

    inner = ScriptedProvider(["not json", "still not json"])
    provider = CachingProvider(inner, Cache(tmp_path))
    with pytest.raises(llm.InvalidOutputError):
        llm.complete_json(provider, "sys", "user", Answer)
    assert len(inner.calls) == 2

    # The retry reaches the provider, and its good answer is the one kept.
    inner.queue('{"count": 3}')
    assert llm.complete_json(provider, "sys", "user", Answer).count == 3
    assert len(inner.calls) == 3
    assert llm.complete_json(provider, "sys", "user", Answer).count == 3
    assert len(inner.calls) == 3


def test_an_answer_corrected_on_the_second_attempt_stays_cached(tmp_path: Path):
    from pydantic import BaseModel

    class Answer(BaseModel):
        count: int

    inner = ScriptedProvider(["not json", '{"count": 3}'])
    provider = CachingProvider(inner, Cache(tmp_path))
    assert llm.complete_json(provider, "sys", "user", Answer).count == 3
    assert llm.complete_json(provider, "sys", "user", Answer).count == 3
    assert len(inner.calls) == 2


def test_a_batch_that_is_all_stored_makes_no_call_whatever_the_batch_size(tmp_path: Path):
    cache = Cache(tmp_path)
    keys = ["a" * 64, "b" * 64]
    assert cache.get_or_call_many(keys, lambda missing: [k[0] for k in missing]) == ["a", "b"]

    def never(missing: list[str]) -> list[str]:
        raise AssertionError(f"called for {missing}")

    assert cache.get_or_call_many(keys, never) == ["a", "b"]
