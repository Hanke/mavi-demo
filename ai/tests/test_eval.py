"""The eval over the fake provider: it scores every fixture, and a second run
is served entirely from the response cache."""

from __future__ import annotations

from pathlib import Path

import pytest

from app import eval as evalmod
from app import fixtures
from app.cache import Cache
from app.fake import FakeProvider
from app.llm import CachingProvider, ProviderError, ScriptedProvider
from app.settings import get_settings


def _run(directory: Path, mode: str = "on") -> tuple[evalmod.Report, Cache]:
    cache = Cache(directory, mode)  # pyright: ignore[reportArgumentType]
    return evalmod.run(CachingProvider(FakeProvider(), cache)), cache


def test_second_run_of_the_eval_makes_no_provider_calls(tmp_path: Path):
    jds = fixtures.load_jds()
    counts = {
        "resumes": len(fixtures.resume_slugs()),
        "jds": len(jds),
        "rerank": sum(1 for jd in jds if jd.hard_filter_matches),
    }
    expected_calls = sum(counts.values())

    first, cold = _run(tmp_path)
    assert (cold.calls, cold.hits) == (expected_calls, 0)
    second, warm = _run(tmp_path)
    assert (warm.calls, warm.hits) == (0, expected_calls)
    assert second == first
    assert not first.errors
    assert first.counts == counts


def test_the_bypass_flag_calls_the_provider_again(tmp_path: Path):
    _, cold = _run(tmp_path)
    _, bypass = _run(tmp_path, mode="off")
    assert (bypass.calls, bypass.hits) == (cold.calls, 0)


def test_scores_are_means_over_the_fixtures(tmp_path: Path):
    report, _ = _run(tmp_path)
    assert report.sections["resumes"]["email"] == 1.0
    assert all(0.0 <= v <= 1.0 for scores in report.sections.values() for v in scores.values())


def test_a_failing_case_is_reported_not_raised():
    provider = ScriptedProvider([ProviderError("down")] * 100)
    report = evalmod.run(provider)
    assert len(report.errors) == len(provider.calls)
    assert report.errors[0].startswith("resumes/")
    assert set(report.sections["resumes"].values()) == {0.0}


def test_cli_reports_zero_calls_on_the_second_run(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
):
    monkeypatch.setenv("AI_CACHE_DIR", str(tmp_path))
    monkeypatch.setenv("LLM_PROVIDER", "fake")
    get_settings.cache_clear()
    try:
        assert evalmod.main([]) == 0
        first = capsys.readouterr().out
        assert evalmod.main([]) == 0
        second = capsys.readouterr().out
        assert evalmod.main(["--no-cache"]) == 0
        bypassed = capsys.readouterr().out
    finally:
        get_settings.cache_clear()
    assert "provider: fake" in first
    assert "provider calls: 0 " not in first
    assert "provider calls: 0 " in second
    assert first.splitlines()[1:-1] == second.splitlines()[1:-1]
    assert bypassed.splitlines()[-1] == first.splitlines()[-1]
