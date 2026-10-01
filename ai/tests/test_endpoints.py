"""/parse-resume, /parse-jd and /rerank over a ScriptedProvider, using the
committed fixtures as the model's "answers" so the round trip through the
request and response models is exercised on realistic data."""

from __future__ import annotations

import json
from typing import Any

import pytest
from fastapi.testclient import TestClient

from app import extract, fixtures, llm, rubric
from app.llm import ProviderError, ScriptedProvider
from app.main import app, get_provider
from app.settings import Settings, get_settings

app.dependency_overrides[get_settings] = lambda: Settings(embedding_provider="local", embedding_dim=8)
client = TestClient(app)


@pytest.fixture
def provider():
    fake = ScriptedProvider([])
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


def test_parse_resume_returns_the_validated_profile(provider: ScriptedProvider):
    fixture = fixtures.load_resume("senior_accountant_cpa_netsuite")
    provider.queue(_resume_answer(fixture.slug))
    resp = client.post("/parse-resume", json={"text": fixture.text, "as_of": fixtures.AS_OF.isoformat()})
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["provider"] == "scripted"
    assert body["contact"] == fixture.raw["contact"]
    assert body["profile"] == fixture.raw["profile"]
    system, user, schema = provider.calls[0]
    assert fixture.text in user
    assert f"Today is {fixtures.AS_OF.isoformat()}" in user
    assert "taxonomy" in system.lower() or "enum" in system.lower()
    # The taxonomy ids travel to the model as an enum on the list fields.
    profile_schema = schema["$defs"]["CandidateProfile"]
    assert "cpa" in profile_schema["properties"]["certifications"]["items"]["enum"]


def test_parse_resume_resolves_aliases_the_model_used(provider: ScriptedProvider):
    """The model answered with display names; the profile comes back with ids."""
    fixture = fixtures.load_resume("senior_accountant_cpa_netsuite")
    answer = json.loads(_resume_answer(fixture.slug))
    answer["profile"]["software"] = ["NetSuite", "Microsoft Excel"]
    answer["profile"]["certifications"] = ["Certified Public Accountant (CPA)"]
    provider.queue(json.dumps(answer))
    resp = client.post("/parse-resume", json={"text": fixture.text})
    assert resp.status_code == 200, resp.text
    # The alias resolves to the ambiguous "cpa"; the qualification record (a
    # California licence) settles it as the US one.
    assert resp.json()["profile"]["certifications"] == ["cpa_us"]
    assert "netsuite" in resp.json()["profile"]["software"]


def test_parse_resume_retries_then_errors_on_invalid_output(provider: ScriptedProvider):
    provider.queue('{"contact": {"full_name": "A"}, "profile": {"years_experience": -1}}', "garbage")
    resp = client.post("/parse-resume", json={"text": "some resume"})
    assert resp.status_code == 502
    detail = resp.json()["detail"]
    assert detail.startswith("llm output invalid")
    assert f"after {llm.MAX_ATTEMPTS} attempts" in detail
    assert len(provider.calls) == llm.MAX_ATTEMPTS
    assert "profile.years_experience" in provider.calls[1][1]


def test_parse_resume_provider_failure_is_a_clear_error(provider: ScriptedProvider):
    provider.queue(ProviderError("anthropic: rate limited"))
    resp = client.post("/parse-resume", json={"text": "some resume"})
    assert resp.status_code == 502
    assert resp.json()["detail"] == "llm provider error: anthropic: rate limited"


def test_parse_resume_rejects_bad_requests_before_calling_the_model(provider: ScriptedProvider):
    assert client.post("/parse-resume", json={"text": ""}).status_code == 422
    assert client.post("/parse-resume", json={}).status_code == 422
    assert client.post("/parse-resume", json={"text": "x", "as_of": "yesterday"}).status_code == 422
    assert provider.calls == []


# --- /parse-jd -----------------------------------------------------------------


def test_parse_jd_returns_the_validated_requirements(provider: ScriptedProvider):
    fixture = fixtures.load_jd("senior_accountant_strict")
    provider.queue(_jd_answer(fixture.slug))
    resp = client.post("/parse-jd", json={"text": fixture.text})
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["company"] == fixture.company
    assert body["requirements"] == fixture.raw["requirements"]
    assert fixture.text in provider.calls[0][1]


def test_parse_jd_never_passes_through_unknown_fields(provider: ScriptedProvider):
    fixture = fixtures.load_jd("vague_finance_generalist")
    answer = json.loads(_jd_answer(fixture.slug))
    answer["requirements"]["salary"] = "lots"
    provider.queue(json.dumps(answer), json.dumps(answer))
    resp = client.post("/parse-jd", json={"text": fixture.text})
    assert resp.status_code == 502
    assert "salary" in resp.json()["detail"]


# --- /rerank -------------------------------------------------------------------


def _rerank_request() -> tuple[dict[str, Any], list[str]]:
    """A request body and the candidate ids it carries."""
    jd = fixtures.load_jd("senior_accountant_strict")
    resumes = fixtures.load_resumes()[:3]
    body: dict[str, Any] = {"role": jd.text, "candidates": [{"id": r.slug, "text": r.text} for r in resumes]}
    return body, [r.slug for r in resumes]


def _judgement(
    cid: str, levels: tuple[int | None, int, int | None, int | None, int | None], *reasons: str
) -> dict[str, Any]:
    """A model answer for one candidate: a level per rubric dimension, in rubric order."""
    keys = ("must_have_coverage", "experience_depth", "software_fluency", "industry_fit", "nice_to_haves")
    quotes = [_first_line(cid)] if cid in fixtures.resume_slugs() else []
    dimensions = {
        k: {"evidence": "e", "quotes": quotes if lv else [], "level": lv} for k, lv in zip(keys, levels, strict=True)
    }
    return {"id": cid, "dimensions": dimensions, "reasons": list(reasons)}


def _first_line(slug: str) -> str:
    """Something the resume really says, for a quote that passes the check."""
    return next(line.strip() for line in fixtures.load_resume(slug).text.splitlines() if line.strip())


def _answer(*judgements: dict[str, Any]) -> str:
    return json.dumps({"results": list(judgements)})


def _with_quotes(judgement: dict[str, Any], key: str, *quotes: str) -> dict[str, Any]:
    """The judgement with the quotes of one dimension replaced."""
    out = json.loads(json.dumps(judgement))
    out["dimensions"][key]["quotes"] = list(quotes)
    return out


def _assert_every_quote_is_in_its_resume(req: dict[str, Any], results: list[dict[str, Any]]) -> None:
    texts = {c["id"]: c["text"] for c in req["candidates"]}
    for r in results:
        for d in r["dimensions"].values():
            assert all(q in texts[r["id"]] for q in d["quotes"])


def test_rerank_scores_every_candidate_best_first(provider: ScriptedProvider):
    req, ids = _rerank_request()
    provider.queue(
        json.dumps(
            {
                "results": [
                    _judgement(ids[0], (1, 4, 4, 4, 4), "no CPA"),
                    _judgement(ids[1], (4, 3, 4, 2, 4), "CPA", "NetSuite"),
                    _judgement(ids[2], (2, 3, 2, 2, 2)),
                ]
            }
        )
    )
    resp = client.post("/rerank", json=req)
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert [r["id"] for r in body["results"]] == [ids[1], ids[2], ids[0]]
    assert body["results"][0]["reasons"] == ["CPA", "NetSuite"]
    assert body["provider"] == "scripted"
    assert body["rubric_version"] == rubric.VERSION
    user = provider.calls[0][1]
    assert all(i in user for i in ids)
    assert provider.calls[0][0] == extract.RERANK_SYSTEM


def test_rerank_score_is_computed_from_the_dimension_levels(provider: ScriptedProvider):
    req, ids = _rerank_request()
    provider.queue(
        json.dumps(
            {
                "results": [
                    _judgement(ids[0], (1, 4, 4, 4, None)),  # 0.667 before the must-have cap
                    _judgement(ids[1], (3, 3, 2, 4, None)),
                    _judgement(ids[2], (1, 2, 4, 4, None)),  # same cap as ids[0], weaker before it
                ]
            }
        )
    )
    results = client.post("/rerank", json=req).json()["results"]
    assert [(r["id"], r["score"]) for r in results] == [(ids[1], 0.736), (ids[0], 0.5), (ids[2], 0.5)]
    for r in results:
        levels = {key: d["level"] for key, d in r["dimensions"].items()}
        assert r["score"] == rubric.overall(levels)
        assert all(d["evidence"] for d in r["dimensions"].values())


def test_rerank_refuses_a_score_from_the_model(provider: ScriptedProvider):
    req, ids = _rerank_request()
    bad = json.dumps({"results": [_judgement(i, (4, 4, 4, 4, 4)) | {"score": 0.1} for i in ids]})
    provider.queue(bad, bad)
    resp = client.post("/rerank", json=req)
    assert resp.status_code == 502
    assert "score" in resp.json()["detail"]


def test_rerank_retries_when_ids_are_missing_or_invented(provider: ScriptedProvider):
    req, ids = _rerank_request()
    good: dict[str, object] = {"results": [_judgement(i, (2, 2, 2, 2, 2)) for i in ids]}
    bad: dict[str, object] = {"results": [_judgement(ids[0], (2, 2, 2, 2, 2)), _judgement("nobody", (0, 0, 0, 0, 0))]}
    provider.queue(json.dumps(bad), json.dumps(good))
    resp = client.post("/rerank", json=req)
    assert resp.status_code == 200, resp.text
    assert sorted(r["id"] for r in resp.json()["results"]) == sorted(ids)
    retry_prompt = provider.calls[1][1]
    assert "exactly once" in retry_prompt
    assert "nobody" in retry_prompt


def test_rerank_retries_when_a_dimension_applies_to_some_candidates_only(provider: ScriptedProvider):
    req, ids = _rerank_request()
    mixed = {"results": [_judgement(ids[0], (2, 2, 2, 2, None)), *(_judgement(i, (2, 2, 2, 2, 2)) for i in ids[1:])]}
    good = {"results": [_judgement(i, (2, 2, 2, 2, None)) for i in ids]}
    provider.queue(json.dumps(mixed), json.dumps(good))
    resp = client.post("/rerank", json=req)
    assert resp.status_code == 200, resp.text
    assert "nice_to_haves must be null for every candidate or for none" in provider.calls[1][1]


def test_rerank_quotes_come_back_as_the_resume_writes_them(provider: ScriptedProvider):
    """Case, line breaks and dash style may differ; what is returned is cut from the resume."""
    role = "Senior Accountant"
    text = (
        "Jo Doe\nSenior Accountant, Acme \u2013 2019 to present\n"
        "- Led the month-end close\n  across 4 entities in NetSuite"
    )
    quoted = _with_quotes(
        {
            "id": "jo",
            "dimensions": {k: {"evidence": "e", "quotes": [], "level": 0} for k in rubric.BY_KEY},
            "reasons": [],
        },
        "experience_depth",
        "senior accountant, acme - 2019 to present",
        "Led the month-end close across 4 entities in NetSuite",
        "Led the month-end close   across 4 entities in NetSuite",  # the same passage again
    )
    quoted["dimensions"]["experience_depth"]["level"] = 3
    provider.queue(_answer(quoted))
    resp = client.post("/rerank", json={"role": role, "candidates": [{"id": "jo", "text": text}]})
    assert resp.status_code == 200, resp.text
    quotes = resp.json()["results"][0]["dimensions"]["experience_depth"]["quotes"]
    assert quotes == [
        "Senior Accountant, Acme \u2013 2019 to present",
        "Led the month-end close\n  across 4 entities in NetSuite",
    ]
    assert all(q in text for q in quotes)
    assert len(provider.calls) == 1


def test_rerank_retries_when_a_quote_is_not_in_the_resume(provider: ScriptedProvider):
    req, ids = _rerank_request()
    good = [_judgement(i, (2, 2, 2, 2, 2)) for i in ids]
    invented = _with_quotes(good[0], "software_fluency", "Implemented SAP S/4HANA across 40 entities")
    provider.queue(_answer(invented, *good[1:]), _answer(*good))
    resp = client.post("/rerank", json=req)
    assert resp.status_code == 200, resp.text
    assert len(provider.calls) == 2
    retry_prompt = provider.calls[1][1]
    assert f"{ids[0]}, software_fluency: not in the candidate's text" in retry_prompt
    assert "Implemented SAP S/4HANA across 40 entities" in retry_prompt
    _assert_every_quote_is_in_its_resume(req, resp.json()["results"])


def test_rerank_drops_a_quote_that_is_still_fabricated_after_the_retry(
    provider: ScriptedProvider, caplog: pytest.LogCaptureFixture
):
    req, ids = _rerank_request()
    good = [_judgement(i, (2, 2, 2, 2, 2)) for i in ids]
    real = _first_line(ids[0])
    invented = _with_quotes(good[0], "software_fluency", "Implemented SAP S/4HANA across 40 entities", real)
    provider.queue(_answer(invented, *good[1:]), _answer(invented, *good[1:]))
    with caplog.at_level("WARNING", logger="app.extract"):
        resp = client.post("/rerank", json=req)
    assert resp.status_code == 200, resp.text
    results = resp.json()["results"]
    first = next(r for r in results if r["id"] == ids[0])
    assert first["dimensions"]["software_fluency"] == {"evidence": "e", "quotes": [real], "level": 2}
    assert "SAP S/4HANA" not in resp.text
    assert "SAP S/4HANA" in caplog.text
    _assert_every_quote_is_in_its_resume(req, results)


def test_rerank_a_quote_from_another_candidate_is_not_evidence(
    provider: ScriptedProvider, caplog: pytest.LogCaptureFixture
):
    req, ids = _rerank_request()
    good = [_judgement(i, (2, 2, 2, 2, 2)) for i in ids]
    borrowed = _with_quotes(good[0], "industry_fit", _first_line(ids[1]))
    provider.queue(_answer(borrowed, *good[1:]), _answer(borrowed, *good[1:]))
    with caplog.at_level("WARNING", logger="app.extract"):
        resp = client.post("/rerank", json=req)
    assert resp.status_code == 200, resp.text
    assert len(provider.calls) == 2
    first = next(r for r in resp.json()["results"] if r["id"] == ids[0])
    assert first["dimensions"]["industry_fit"]["quotes"] == []
    assert first["dimensions"]["industry_fit"]["level"] == 2  # the judgement stands; only the quote goes
    assert f"{ids[0]}.industry_fit" in caplog.text.split("no quote to back the level: ")[1]


def test_rerank_asks_again_for_a_scored_dimension_with_no_quote_and_for_an_overlong_one(provider: ScriptedProvider):
    req, ids = _rerank_request()
    good = [_judgement(i, (2, 2, 2, 2, 2)) for i in ids]
    whole_resume = next(c["text"] for c in req["candidates"] if c["id"] == ids[1])
    bad = [_with_quotes(good[0], "experience_depth"), _with_quotes(good[1], "experience_depth", whole_resume), good[2]]
    provider.queue(_answer(*bad), _answer(*good))
    assert client.post("/rerank", json=req).status_code == 200
    problems = provider.calls[1][1].rsplit("Problems:", 1)[1].split("Previous answer:")[0]
    assert f"{ids[0]}, experience_depth: level 2 needs at least one quote" in problems
    assert f"{ids[1]}, experience_depth: 1 longer than {extract.MAX_QUOTE_CHARS} characters" in problems
    assert len(problems) < len(whole_resume)  # the overlong quote is counted, not sent back


def test_rerank_keeps_the_first_answer_when_the_retry_for_a_quote_comes_back_broken(provider: ScriptedProvider):
    req, ids = _rerank_request()
    good = [_judgement(i, (2, 2, 2, 2, 2)) for i in ids]
    invented = _with_quotes(good[0], "software_fluency", "Implemented SAP S/4HANA across 40 entities")
    provider.queue(_answer(invented, *good[1:]), "not json")
    resp = client.post("/rerank", json=req)
    assert resp.status_code == 200, resp.text
    assert "SAP S/4HANA" not in resp.text
    assert sorted(r["id"] for r in resp.json()["results"]) == sorted(ids)


def test_rerank_without_quotes_is_not_valid_output(provider: ScriptedProvider):
    req, ids = _rerank_request()
    bad = [_judgement(i, (2, 2, 2, 2, 2)) for i in ids]
    for j in bad:
        del j["dimensions"]["experience_depth"]["quotes"]
    provider.queue(_answer(*bad), _answer(*bad))
    resp = client.post("/rerank", json=req)
    assert resp.status_code == 502
    assert "experience_depth.quotes" in resp.json()["detail"]


def test_rerank_orders_equal_candidates_by_id_not_by_the_models_order(provider: ScriptedProvider):
    req, ids = _rerank_request()
    for order in (ids, ids[::-1], [ids[1], ids[2], ids[0]]):
        provider.queue(_answer(*(_judgement(i, (3, 3, 3, 3, 3)) for i in order)))
        results = client.post("/rerank", json=req).json()["results"]
        assert [r["id"] for r in results] == sorted(ids)


def test_rerank_does_not_depend_on_the_order_of_the_request(provider: ScriptedProvider):
    req, ids = _rerank_request()
    reversed_req = req | {"candidates": req["candidates"][::-1]}
    answer = _answer(
        _judgement(ids[0], (2, 2, 2, 2, 2)), _judgement(ids[1], (4, 3, 4, 2, 4)), _judgement(ids[2], (2, 2, 2, 2, 2))
    )
    provider.queue(answer, answer)
    first, second = client.post("/rerank", json=req), client.post("/rerank", json=reversed_req)
    assert first.json() == second.json()
    # The same prompt, so the same cache entry and the same answer from a real model.
    assert provider.calls[0] == provider.calls[1]


def test_rerank_level_off_the_scale_is_never_returned(provider: ScriptedProvider):
    req, ids = _rerank_request()
    for levels in ((7, 2, 2, 2, 2), (2, None, 2, 2, 2), (2, 2.5, 2, 2, 2)):
        bad = json.dumps({"results": [_judgement(i, levels) for i in ids]})  # pyright: ignore[reportArgumentType]
        provider.queue(bad, bad)
        resp = client.post("/rerank", json=req)
        assert resp.status_code == 502
        assert "level" in resp.json()["detail"]


def test_rerank_rejects_duplicate_ids_and_empty_lists(provider: ScriptedProvider):
    dup = {"role": "r", "candidates": [{"id": "a", "text": "x"}, {"id": "a", "text": "y"}]}
    assert client.post("/rerank", json=dup).status_code == 422
    assert client.post("/rerank", json={"role": "r", "candidates": []}).status_code == 422
    assert provider.calls == []


def test_error_detail_is_bounded(provider: ScriptedProvider):
    """A 50-candidate rerank can produce hundreds of validation lines; the Go
    client reads at most 4KB of an error body, so the detail must stay short."""
    from app.main import MAX_ERROR_DETAIL_CHARS

    req, ids = _rerank_request()
    bad = json.dumps({"results": [_judgement(i, (7, 7, 7, 7, 7)) | {"junk": "x" * 500} for i in ids]})
    provider.queue(bad, bad)
    resp = client.post("/rerank", json=req)
    assert resp.status_code == 502
    detail = resp.json()["detail"]
    assert len(detail) <= MAX_ERROR_DETAIL_CHARS
    assert detail.startswith("llm output invalid")


def test_rejected_output_is_logged(provider: ScriptedProvider, caplog: pytest.LogCaptureFixture):
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
