"""CLI: python -m app.eval [--provider NAME] [--no-cache]

Runs the parsers and the reranker over the hand-written fixtures in
infra/fixtures and scores what comes back against the expected outputs:

  resumes  /parse-resume's extraction, field by field
  jds      /parse-jd's extraction, field by field
  rerank   for each JD with hard-filter matches, how many of the top k ranked
           resumes are among its k matches

Exact fields score 1 or 0; list fields score F1 against the expected ids.
Each number is the mean over the fixtures. A case the provider fails on
scores 0 everywhere and is listed.

The provider is the one the settings select (`LLM_PROVIDER`, or --provider),
behind the response cache, and the last line says how many calls reached it:
a second run over unchanged fixtures and prompts reports 0. --no-cache
bypasses the cache for this run.
"""

from __future__ import annotations

import argparse
import sys
from collections.abc import Callable, Iterable
from dataclasses import dataclass, field
from typing import cast

from app import extract, fixtures, llm
from app.extract import RerankCandidate
from app.grounding import normalize
from app.llm import LLMError, Provider
from app.schemas import CandidateProfile, Contact, RoleRequirements
from app.settings import get_settings

RESUME_FIELDS = (
    "full_name",
    "email",
    "phone",
    "location",
    "years_experience",
    "positions",
    "certifications",
    "qualifications",
    "software",
    "industries",
    "gaap_exposure",
    "availability",
    "timezone",
)
JD_FIELDS = (
    "company",
    "title",
    "required_certifications",
    "required_qualifications",
    "required_software",
    "min_years_experience",
    "preferred_certifications",
    "preferred_software",
    "industries",
    "must_haves",
    "nice_to_haves",
    "timezone",
    "starts_on",
)


@dataclass
class Report:
    """section -> field -> mean score in 0..1, plus the cases that errored."""

    sections: dict[str, dict[str, float]] = field(default_factory=dict[str, dict[str, float]])
    counts: dict[str, int] = field(default_factory=dict[str, int])
    errors: list[str] = field(default_factory=list[str])


def _same(got: object, want: object) -> float:
    def norm(v: object) -> object:
        return v.strip().lower() if isinstance(v, str) else v

    return float(norm(got) == norm(want))


def _f1(got: Iterable[str], want: Iterable[str]) -> float:
    g, w = set(got), set(want)
    if not g and not w:
        return 1.0
    return 2 * len(g & w) / (len(g) + len(w))


def _score(got: object, want: object) -> float:
    if isinstance(want, list):
        return _f1(cast(list[str], got), cast(list[str], want))
    return _same(got, want)


def _section[F](
    report: Report,
    name: str,
    fields: tuple[str, ...],
    cases: list[F],
    *,
    slug: Callable[[F], str],
    run: Callable[[F], dict[str, float]],
) -> None:
    totals = dict.fromkeys(fields, 0.0)
    for case in cases:
        try:
            scores = run(case)
        except LLMError as e:
            report.errors.append(f"{name}/{slug(case)}: {e}")
            continue
        for f in fields:
            totals[f] += scores[f]
    report.sections[name] = {f: totals[f] / len(cases) for f in fields} if cases else {}
    report.counts[name] = len(cases)


def _resume_values(contact: Contact, profile: CandidateProfile) -> dict[str, object]:
    """Contact and profile as one flat dict, with the free-form lists made
    comparable: a position is its title and years, a standard its lower-cased
    name, a qualification what it is, where it is from and whether it is held."""
    return {
        **contact.model_dump(),
        **profile.model_dump(),
        "positions": [f"{p.title.lower()} {p.start_year}-{p.end_year}" for p in profile.positions],
        "gaap_exposure": [g.lower() for g in profile.gaap_exposure],
        "qualifications": [
            f"{q.canonical or q.name_as_written.lower()} {q.jurisdiction} {q.status}" for q in profile.qualifications
        ],
    }


def _jd_values(company: str | None, req: RoleRequirements) -> dict[str, object]:
    """As above for a JD: a required qualification is its id and whether
    equivalents will do, a must-have or nice-to-have its normalised text."""
    return {
        "company": company,
        **req.model_dump(),
        "required_qualifications": [
            f"{q.canonical or q.name_as_written.lower()} {q.accept_equivalents}" for q in req.required_qualifications
        ],
        # Verbatim fragments: the same line with different case or a trailing full stop is the same line.
        "must_haves": [normalize(m).rstrip(".") for m in req.must_haves],
        "nice_to_haves": [normalize(n).rstrip(".") for n in req.nice_to_haves],
    }


def run(provider: Provider) -> Report:
    report = Report()
    resumes = fixtures.load_resumes()
    jds = fixtures.load_jds()

    def resume(fx: fixtures.ResumeFixture) -> dict[str, float]:
        out = extract.parse_resume(provider, fx.text, fixtures.AS_OF)
        got = _resume_values(out.contact, out.profile)
        want = _resume_values(fx.contact, fx.expected)
        return {f: _score(got[f], want[f]) for f in RESUME_FIELDS}

    def jd(fx: fixtures.JDFixture) -> dict[str, float]:
        out = extract.parse_jd(provider, fx.text)
        got = _jd_values(out.company, out.requirements)
        want = _jd_values(fx.company, fx.expected)
        return {f: _score(got[f], want[f]) for f in JD_FIELDS}

    candidates = [RerankCandidate(id=r.slug, text=r.text) for r in resumes]

    def rerank(fx: fixtures.JDFixture) -> dict[str, float]:
        ranked = extract.rerank(provider, fx.text, candidates)
        k = len(fx.hard_filter_matches)
        top = {r.id for r in ranked[:k]}
        return {"precision_at_k": len(top & set(fx.hard_filter_matches)) / k}

    _section(report, "resumes", RESUME_FIELDS, resumes, slug=lambda fx: fx.slug, run=resume)
    _section(report, "jds", JD_FIELDS, jds, slug=lambda fx: fx.slug, run=jd)
    rankable = [fx for fx in jds if fx.hard_filter_matches]
    _section(report, "rerank", ("precision_at_k",), rankable, slug=lambda fx: fx.slug, run=rerank)
    return report


def render(report: Report) -> str:
    lines: list[str] = []
    for name, scores in report.sections.items():
        lines.append(f"{name} ({report.counts[name]})")
        lines.extend(f"  {f:<26}{score:.2f}" for f, score in scores.items())
    if report.errors:
        lines.append(f"errors ({len(report.errors)})")
        lines.extend(f"  {e}" for e in report.errors)
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="python -m app.eval", description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    parser.add_argument("--provider", choices=["anthropic", "openai", "fake"], help="override LLM_PROVIDER")
    parser.add_argument("--no-cache", action="store_true", help="bypass the response cache (same as AI_CACHE=off)")
    args = parser.parse_args(argv)

    update: dict[str, str] = {}
    if cast(str | None, args.provider):
        update["llm_provider"] = cast(str, args.provider)
    if cast(bool, args.no_cache):
        update["ai_cache"] = "off"
    settings = get_settings().model_copy(update=update)
    provider = llm.build_provider(settings)
    cache = provider.cache

    print(f"provider: {provider.name} ({provider.model})   cache: {cache.mode} ({cache.directory})")
    report = run(provider)
    print(render(report))
    print(f"provider calls: {cache.calls}   cache hits: {cache.hits}")
    return 1 if report.errors else 0


if __name__ == "__main__":
    sys.exit(main())
