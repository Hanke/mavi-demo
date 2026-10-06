"""What a request spent on the chat model: /rerank reports its calls, tokens
and estimated cost, a replay from the cache is counted as one and costs
nothing, and the cost is null when no price is known."""

from __future__ import annotations

import pytest
from fastapi.testclient import TestClient

from app import fixtures, usage
from app.main import app
from app.settings import Settings, get_settings

ROLE = fixtures.load_jd("senior_accountant_strict").text
CANDIDATES = [
    {"id": slug, "text": fixtures.load_resume(slug).text}
    for slug in ("senior_accountant_cpa_netsuite", "bookkeeper_part_time")
]
RERANK: dict[str, object] = {"role": ROLE, "candidates": CANDIDATES}


@pytest.fixture
def client():
    app.dependency_overrides[get_settings] = lambda: Settings(
        llm_provider="fake", embedding_provider="local", embedding_dim=8
    )
    yield TestClient(app)
    app.dependency_overrides[get_settings] = lambda: Settings(embedding_provider="local", embedding_dim=8)


def test_rerank_reports_what_it_spent_and_a_replay_spends_nothing(client: TestClient):
    first = client.post("/rerank", json=RERANK).json()
    spent = first["usage"]
    assert (spent["llm_calls"], spent["cache_hits"]) == (1, 0)
    # The fake has no tokenizer: its counts are the size of the prompt and the answer.
    prompt_chars = len(ROLE) + sum(len(c["text"]) for c in CANDIDATES)
    assert spent["input_tokens"] > prompt_chars // 4
    assert spent["output_tokens"] > 0
    assert spent["estimated_cost_usd"] == 0.0  # the fake provider is free
    assert spent["model"].startswith("fake-")

    again = client.post("/rerank", json=RERANK).json()
    assert again["results"] == first["results"]
    assert again["usage"] == {**spent, "llm_calls": 0, "cache_hits": 1, "input_tokens": 0, "output_tokens": 0}


def test_one_requests_usage_does_not_leak_into_the_next(client: TestClient):
    client.post("/rerank", json=RERANK)
    other = {**RERANK, "candidates": CANDIDATES[:1]}
    assert client.post("/rerank", json=other).json()["usage"]["llm_calls"] == 1


def test_cost_is_tokens_at_the_models_list_price():
    meter = usage.Meter(completions=3, cache_hits=1, input_tokens=100_000, output_tokens=10_000)
    report = usage.report("anthropic", "claude-opus-5-5", Settings(), meter)
    # $4 and $20 per million tokens.
    assert report.estimated_cost_usd == pytest.approx(0.4 + 0.2)
    assert (report.llm_calls, report.cache_hits, report.model) == (2, 1, "claude-opus-5-5")


def test_cost_is_null_for_a_model_with_no_known_price_unless_one_is_configured():
    meter = usage.Meter(completions=1, input_tokens=1_000_000, output_tokens=1_000_000)
    assert usage.report("openai", "some-model", Settings(), meter).estimated_cost_usd is None
    priced = Settings(llm_input_price_per_mtok=1.5, llm_output_price_per_mtok=6)
    assert usage.report("openai", "some-model", priced, meter).estimated_cost_usd == pytest.approx(7.5)
    # A configured price wins over the table; the fake is free whatever is configured.
    assert usage.report("anthropic", "claude-opus-5-5", priced, meter).estimated_cost_usd == pytest.approx(7.5)
    assert usage.report("fake", "fake-7", priced, meter).estimated_cost_usd == 0.0


def test_an_unset_price_arrives_from_compose_as_an_empty_string(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setenv("LLM_INPUT_PRICE_PER_MTOK", "")
    monkeypatch.setenv("LLM_OUTPUT_PRICE_PER_MTOK", " ")
    settings = Settings()
    assert settings.llm_input_price_per_mtok is None
    assert settings.llm_output_price_per_mtok is None


def test_nothing_is_counted_outside_a_request():
    usage.completion()
    usage.cache_hit()
    usage.tokens(10, 10)  # no meter: no error, nothing kept
