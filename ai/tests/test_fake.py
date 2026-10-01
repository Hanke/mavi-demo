"""LLM_PROVIDER=fake: the key-free provider answers all three chat endpoints
with output that passes the same validation a model's would, the same way
every time. Every fixture goes through it, so a change to the prompts in
app/extract.py that the fake can no longer read fails here."""

from __future__ import annotations

import pytest
from fastapi.testclient import TestClient

from app import fixtures, llm, rubric
from app.fake import FakeProvider
from app.llm import ProviderError
from app.main import app, get_provider
from app.settings import Settings, get_settings


@pytest.fixture
def client():
    app.dependency_overrides[get_settings] = lambda: Settings(
        llm_provider="fake", embedding_provider="local", embedding_dim=8
    )
    yield TestClient(app)
    app.dependency_overrides[get_settings] = lambda: Settings(embedding_provider="local", embedding_dim=8)


def test_fake_is_selected_by_the_setting_and_needs_no_key(client: TestClient):
    provider = llm.build_provider(Settings(llm_provider="fake", anthropic_api_key="", openai_api_key=""))
    assert isinstance(provider.inner, FakeProvider)
    assert client.get("/health").json()["llm_provider"] == "fake"


@pytest.mark.parametrize("slug", fixtures.resume_slugs())
def test_parse_resume_with_the_fake_is_valid_and_repeatable(client: TestClient, slug: str):
    fixture = fixtures.load_resume(slug)
    req = {"text": fixture.text, "as_of": fixtures.AS_OF.isoformat()}
    resp = client.post("/parse-resume", json=req)
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["provider"] == "fake"
    assert body["contact"]["email"] == fixture.contact.email
    assert body == client.post("/parse-resume", json=req).json()
    # Straight from the provider too, so the repeat above is not just the cache.
    assert _complete(FakeProvider(), req["text"]) == _complete(FakeProvider(), req["text"])


def _complete(provider: FakeProvider, text: str) -> str:
    from app.extract import ResumeExtraction, resume_prompt

    return provider.complete("sys", resume_prompt(text, fixtures.AS_OF), llm.output_schema(ResumeExtraction))


def test_parse_resume_finds_the_taxonomy_terms_the_text_names(client: TestClient):
    fixture = fixtures.load_resume("senior_accountant_cpa_netsuite")
    profile = client.post("/parse-resume", json={"text": fixture.text}).json()["profile"]
    assert "cpa_us" in profile["certifications"]  # "CPA" on a resume from Oakland, CA
    assert "netsuite" in profile["software"]


@pytest.mark.parametrize("slug", fixtures.jd_slugs())
def test_parse_jd_with_the_fake_is_valid(client: TestClient, slug: str):
    fixture = fixtures.load_jd(slug)
    resp = client.post("/parse-jd", json={"text": fixture.text})
    assert resp.status_code == 200, resp.text
    assert resp.json()["requirements"]["title"]


def test_parse_jd_keeps_preferred_items_out_of_the_requirements(client: TestClient):
    fixture = fixtures.load_jd("senior_accountant_strict")
    req = client.post("/parse-jd", json={"text": fixture.text}).json()["requirements"]
    assert req["required_certifications"] == ["cpa_us"]
    assert [(q["canonical"], q["accept_equivalents"]) for q in req["required_qualifications"]] == [("cpa_us", False)]
    assert {"netsuite", "blackline", "excel"} <= set(req["required_software"])
    assert "floqast" not in req["required_software"]  # named under "Preferred" only
    assert req["must_haves"] == fixture.expected.must_haves
    assert req["nice_to_haves"] == fixture.expected.nice_to_haves


def test_rerank_with_the_fake_scores_every_candidate_and_ranks_the_match_first(client: TestClient):
    jd = fixtures.load_jd("senior_accountant_strict")
    resumes = fixtures.load_resumes()
    resp = client.post(
        "/rerank", json={"role": jd.text, "candidates": [{"id": r.slug, "text": r.text} for r in resumes]}
    )
    assert resp.status_code == 200, resp.text
    results = resp.json()["results"]
    assert sorted(r["id"] for r in results) == sorted(r.slug for r in resumes)
    assert results[0]["id"] == jd.hard_filter_matches[0]
    assert all(r["reasons"] for r in results)
    assert all(r["score"] == rubric.overall({k: d["level"] for k, d in r["dimensions"].items()}) for r in results)


@pytest.mark.parametrize("slug", fixtures.jd_slugs())
def test_rerank_with_the_fake_quotes_the_resumes_and_is_repeatable(client: TestClient, slug: str):
    jd = fixtures.load_jd(slug)
    resumes = fixtures.load_resumes()
    texts = {r.slug: r.text for r in resumes}
    candidates = [{"id": r.slug, "text": r.text} for r in resumes]
    provider = FakeProvider()
    app.dependency_overrides[get_provider] = lambda: provider  # no cache: every request reaches the provider
    try:
        resp = client.post("/rerank", json={"role": jd.text, "candidates": candidates})
        again = client.post("/rerank", json={"role": jd.text, "candidates": candidates[::-1]})
    finally:
        app.dependency_overrides.pop(get_provider, None)
    assert resp.status_code == 200, resp.text
    assert resp.content == again.content
    quoted = 0
    for r in resp.json()["results"]:
        for d in r["dimensions"].values():
            assert all(q in texts[r["id"]] for q in d["quotes"])
            assert bool(d["quotes"]) == bool(d["level"])  # every scored dimension is backed, nothing was dropped
            quoted += len(d["quotes"])
    assert quoted


def test_unknown_schema_is_a_provider_error():
    with pytest.raises(ProviderError, match="no answer defined"):
        FakeProvider().complete("s", "u", {"title": "SomethingElse"})
