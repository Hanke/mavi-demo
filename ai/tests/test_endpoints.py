"""/parse-resume, /parse-jd and /rerank over a FakeProvider, using the
committed fixtures as the model's "answers" so the round trip through the
request and response models is exercised on realistic data."""

from __future__ import annotations

import json

import pytest
from fastapi.testclient import TestClient

from app import fixtures, llm
from app.llm import FakeProvider, ProviderError
from app.main import app, get_provider
from app.settings import Settings, get_settings

app.dependency_overrides[get_settings] = lambda: Settings(embedding_provider="local", embedding_dim=8)
client = TestClient(app)


@pytest.fixture
def provider():
    fake = FakeProvider([])
    app.dependency_overrides[get_provider] = lambda: fake
    yield fake
    app.dependency_overrides.pop(get_provider, None)


def _resume_answer(slug: str) -> str:
    raw = fixtures.load_resume(slug).raw
    return json.dumps({"contact": raw["contact"], "profile": raw["profile"]})


def _jd_answer(slug: str) -> str:
    raw = fixtures.load_jd(slug).raw
    return json.dumps({"company": raw["company"], "requirements": raw["requirements"]})


# --- /parse-resume -------------------------------------------------------------


def test_parse_resume_returns_the_validated_profile(provider: FakeProvider):
    fixture = fixtures.load_resume("senior_accountant_cpa_netsuite")
    provider.queue(_resume_answer(fixture.slug))
    resp = client.post("/parse-resume", json={"text": fixture.text, "as_of": fixtures.AS_OF.isoformat()})
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["provider"] == "fake"
    assert body["contact"] == fixture.raw["contact"]
    assert body["profile"] == fixture.raw["profile"]
    system, user, schema = provider.calls[0]
    assert fixture.text in user
    assert f"Today is {fixtures.AS_OF.isoformat()}" in user
    assert "taxonomy" in system.lower() or "enum" in system.lower()
    # The taxonomy ids travel to the model as an enum on the list fields.
    profile_schema = schema["$defs"]["CandidateProfile"]
    assert "cpa" in profile_schema["properties"]["certifications"]["items"]["enum"]


def test_parse_resume_resolves_aliases_the_model_used(provider: FakeProvider):
    """The model answered with display names; the profile comes back with ids."""
    fixture = fixtures.load_resume("senior_accountant_cpa_netsuite")
    answer = json.loads(_resume_answer(fixture.slug))
    answer["profile"]["software"] = ["NetSuite", "Microsoft Excel"]
    answer["profile"]["certifications"] = ["Certified Public Accountant (CPA)"]
    provider.queue(json.dumps(answer))
    resp = client.post("/parse-resume", json={"text": fixture.text})
    assert resp.status_code == 200, resp.text
    assert resp.json()["profile"]["certifications"] == ["cpa"]
    assert "netsuite" in resp.json()["profile"]["software"]


def test_parse_resume_retries_then_errors_on_invalid_output(provider: FakeProvider):
    provider.queue('{"contact": {"full_name": "A"}, "profile": {}}', "garbage")
    resp = client.post("/parse-resume", json={"text": "some resume"})
    assert resp.status_code == 502
    detail = resp.json()["detail"]
    assert detail.startswith("llm output invalid")
    assert f"after {llm.MAX_ATTEMPTS} attempts" in detail
    assert len(provider.calls) == llm.MAX_ATTEMPTS
    assert "contact.email" in provider.calls[1][1]


def test_parse_resume_provider_failure_is_a_clear_error(provider: FakeProvider):
    provider.queue(ProviderError("anthropic: rate limited"))
    resp = client.post("/parse-resume", json={"text": "some resume"})
    assert resp.status_code == 502
    assert resp.json()["detail"] == "llm provider error: anthropic: rate limited"


def test_parse_resume_rejects_bad_requests_before_calling_the_model(provider: FakeProvider):
    assert client.post("/parse-resume", json={"text": ""}).status_code == 422
    assert client.post("/parse-resume", json={}).status_code == 422
    assert client.post("/parse-resume", json={"text": "x", "as_of": "yesterday"}).status_code == 422
    assert provider.calls == []


# --- /parse-jd -----------------------------------------------------------------


def test_parse_jd_returns_the_validated_requirements(provider: FakeProvider):
    fixture = fixtures.load_jd("senior_accountant_strict")
    provider.queue(_jd_answer(fixture.slug))
    resp = client.post("/parse-jd", json={"text": fixture.text})
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["company"] == fixture.company
    assert body["requirements"] == fixture.raw["requirements"]
    assert fixture.text in provider.calls[0][1]


def test_parse_jd_never_passes_through_unknown_fields(provider: FakeProvider):
    fixture = fixtures.load_jd("vague_finance_generalist")
    answer = json.loads(_jd_answer(fixture.slug))
    answer["requirements"]["salary"] = "lots"
    provider.queue(json.dumps(answer), json.dumps(answer))
    resp = client.post("/parse-jd", json={"text": fixture.text})
    assert resp.status_code == 502
    assert "salary" in resp.json()["detail"]


# --- /rerank -------------------------------------------------------------------


def _rerank_request() -> tuple[dict[str, object], list[str]]:
    """A request body and the candidate ids it carries."""
    jd = fixtures.load_jd("senior_accountant_strict")
    resumes = fixtures.load_resumes()[:3]
    body: dict[str, object] = {"role": jd.text, "candidates": [{"id": r.slug, "text": r.text} for r in resumes]}
    return body, [r.slug for r in resumes]


def test_rerank_scores_every_candidate_best_first(provider: FakeProvider):
    req, ids = _rerank_request()
    provider.queue(
        json.dumps(
            {
                "results": [
                    {"id": ids[0], "score": 0.2, "reasons": ["no CPA"]},
                    {"id": ids[1], "score": 0.9, "reasons": ["CPA", "NetSuite"]},
                    {"id": ids[2], "score": 0.5, "reasons": []},
                ]
            }
        )
    )
    resp = client.post("/rerank", json=req)
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert [r["id"] for r in body["results"]] == [ids[1], ids[2], ids[0]]
    assert body["results"][0]["reasons"] == ["CPA", "NetSuite"]
    assert body["provider"] == "fake"
    user = provider.calls[0][1]
    assert all(i in user for i in ids)


def test_rerank_retries_when_ids_are_missing_or_invented(provider: FakeProvider):
    req, ids = _rerank_request()
    good: dict[str, object] = {"results": [{"id": i, "score": 0.5, "reasons": []} for i in ids]}
    bad: dict[str, object] = {
        "results": [{"id": ids[0], "score": 0.5, "reasons": []}, {"id": "nobody", "score": 0.1, "reasons": []}]
    }
    provider.queue(json.dumps(bad), json.dumps(good))
    resp = client.post("/rerank", json=req)
    assert resp.status_code == 200, resp.text
    assert sorted(r["id"] for r in resp.json()["results"]) == sorted(ids)
    retry_prompt = provider.calls[1][1]
    assert "exactly once" in retry_prompt
    assert "nobody" in retry_prompt


def test_rerank_out_of_range_score_is_never_returned(provider: FakeProvider):
    req, ids = _rerank_request()
    bad = json.dumps({"results": [{"id": i, "score": 7, "reasons": []} for i in ids]})
    provider.queue(bad, bad)
    resp = client.post("/rerank", json=req)
    assert resp.status_code == 502
    assert "score" in resp.json()["detail"]


def test_rerank_rejects_duplicate_ids_and_empty_lists(provider: FakeProvider):
    dup = {"role": "r", "candidates": [{"id": "a", "text": "x"}, {"id": "a", "text": "y"}]}
    assert client.post("/rerank", json=dup).status_code == 422
    assert client.post("/rerank", json={"role": "r", "candidates": []}).status_code == 422
    assert provider.calls == []


def test_error_detail_is_bounded(provider: FakeProvider):
    """A 50-candidate rerank can produce hundreds of validation lines; the Go
    client reads at most 4KB of an error body, so the detail must stay short."""
    from app.main import MAX_ERROR_DETAIL_CHARS

    req, ids = _rerank_request()
    bad = json.dumps({"results": [{"id": i, "score": 7, "reasons": [], "junk": "x" * 500} for i in ids]})
    provider.queue(bad, bad)
    resp = client.post("/rerank", json=req)
    assert resp.status_code == 502
    detail = resp.json()["detail"]
    assert len(detail) <= MAX_ERROR_DETAIL_CHARS
    assert detail.startswith("llm output invalid")


def test_rejected_output_is_logged(provider: FakeProvider, caplog: pytest.LogCaptureFixture):
    provider.queue("nonsense", '{"company": null, "requirements": {"title": "x"}}')
    with caplog.at_level("WARNING", logger="app.llm"):
        resp = client.post("/parse-jd", json={"text": "jd"})
    assert resp.status_code == 200
    assert len(caplog.records) == 1
    assert "JDExtraction" in caplog.records[0].getMessage()
    assert "nonsense" in caplog.records[0].getMessage()


# --- the real providers are only constructed, never called -------------------


def test_default_provider_is_built_from_settings():
    from app.main import get_provider as real_get_provider

    p = real_get_provider(Settings(llm_provider="anthropic", anthropic_api_key="test-key"))
    assert p.name == "anthropic"
    assert real_get_provider(Settings(llm_provider="anthropic", anthropic_api_key="test-key")) is p
