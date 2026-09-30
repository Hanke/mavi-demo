"""The committed seed data (infra/db/seed/data/*.json) and its rendered SQL.

These run in CI without a database: they check that every profile is a valid
CandidateProfile, that the resume text carries the hard-filter values the
profile claims (so evidence quotes have something to point at), that the
distribution is varied enough for hard filters and ranking to change results,
and that the rendered SQL matches the JSON.
"""

from __future__ import annotations

import json
from collections import Counter
from collections.abc import Callable
from pathlib import Path
from typing import Any, cast

import pytest

from app import fixtures, grounding, matching, taxonomy
from app.schemas import CandidateProfile, RoleRequirements
from app.seedgen import generate, plan, render

DATA = Path(__file__).resolve().parents[2] / "infra" / "db" / "seed" / "data"
MIN_CANDIDATES = 150


@pytest.fixture(scope="module")
def candidates() -> list[dict[str, Any]]:
    data = json.loads((DATA / "candidates.json").read_text(encoding="utf-8"))
    return cast(list[dict[str, Any]], data["candidates"])


@pytest.fixture(scope="module")
def roles() -> list[dict[str, Any]]:
    data = json.loads((DATA / "roles.json").read_text(encoding="utf-8"))
    return cast(list[dict[str, Any]], data["roles"])


@pytest.fixture(scope="module")
def profiles(candidates: list[dict[str, Any]]) -> list[CandidateProfile]:
    return [CandidateProfile.model_validate(c["profile"]) for c in candidates]


def test_plan_is_deterministic():
    a = plan.build_plan(count=25)
    b = plan.build_plan(count=25)
    assert a == b
    assert len({s.email for s in a}) == 25
    assert len({s.id for s in a}) == 25


def test_mentions_matches_aliases_but_not_substrings():
    tax = taxonomy.load()
    ca = tax.term("certifications", "ca")
    assert generate.mentions("qualified as a Chartered Accountant in 2019", ca)
    assert generate.mentions("holds the CA designation", ca)
    assert not generate.mentions("moved to California in 2019", ca)
    qb = tax.term("software", "quickbooks")
    assert generate.mentions("kept the books in QBO", qb)
    assert generate.mentions("quickbooks online", qb)
    assert not generate.mentions("used Xero and Sage", qb)


def test_candidates_are_well_formed(candidates: list[dict[str, Any]], profiles: list[CandidateProfile]):
    assert len(candidates) >= MIN_CANDIDATES
    ids = [c["id"] for c in candidates]
    assert len(set(ids)) == len(ids)
    emails = [c["email"].lower() for c in candidates]
    assert len(set(emails)) == len(emails)
    for c, p in zip(candidates, profiles, strict=True):
        assert c["status"] in {"active", "archived"}
        assert c["resume_text"].strip().startswith(c["full_name"]), c["id"]
        assert len(c["resume_text"].split()) >= generate.MIN_WORDS, c["id"]
        assert p.headline
        assert p.years_experience is not None
        assert p.timezone
        days = c["available_in_days"]
        if p.availability in {"unavailable", "unknown"}:
            assert days is None, c["id"]
        else:
            assert isinstance(days, int), c["id"]
            assert days >= 0, c["id"]
        # The stored JSON is exactly the parser's shape, nothing extra.
        assert set(c["profile"]) == set(CandidateProfile.model_fields), c["id"]
        assert len(p.positions) >= generate.MIN_POSITIONS, c["id"]
        # Both are read off the resume text, so they cannot drift from it.
        assert c["profile"]["positions"] == generate.positions_from(c["resume_text"]), c["id"]
        assert p.gaap_exposure == grounding.standards_named(c["resume_text"]), c["id"]
        # Every certification has its record, quoted from the resume.
        assert [q.canonical for q in p.qualifications] == p.certifications, c["id"]
        for q in p.qualifications:
            assert q.status == "qualified", c["id"]
            assert q.quote, c["id"]
            assert q.quote in c["resume_text"], c["id"]
            assert grounding.in_text(q.name_as_written, q.quote), c["id"]


def test_resumes_mention_every_hard_filter_value(candidates: list[dict[str, Any]], profiles: list[CandidateProfile]):
    tax = taxonomy.load()
    missing: list[str] = []
    for c, p in zip(candidates, profiles, strict=True):
        text = c["resume_text"]
        certs = [tax.term("certifications", cid) for cid in p.certifications]
        software = [tax.term("software", sid) for sid in p.software]
        missing += [f"{c['id']}: {t.id}" for t in certs + software if not generate.mentions(text, t)]
    assert not missing, "\n".join(missing)


def test_distribution_is_varied(profiles: list[CandidateProfile]):
    n = len(profiles)

    def share(pred: Callable[[CandidateProfile], bool]) -> float:
        return sum(1 for p in profiles if pred(p)) / n

    # Certifications: enough CPAs to matter, not so many the filter is moot.
    assert 0.2 <= share(lambda p: "cpa_us" in p.certifications) <= 0.5
    assert share(lambda p: not p.certifications) >= 0.3
    assert len({c for p in profiles for c in p.certifications}) >= 8
    # Software: the two named in the brief are common; plenty of others.
    assert 0.2 <= share(lambda p: "quickbooks" in p.software) <= 0.6
    assert 0.1 <= share(lambda p: "netsuite" in p.software) <= 0.5
    assert len({s for p in profiles for s in p.software}) >= 20
    # Availability, timezone, industries, experience.
    assert len({p.availability for p in profiles}) >= 4
    assert len({p.timezone for p in profiles}) >= 6
    assert len({i for p in profiles for i in p.industries}) >= 12
    years = Counter(
        "0-2" if y <= 2 else "3-5" if y <= 5 else "6-10" if y <= 10 else "11-20" if y <= 20 else "20+"
        for y in (p.years_experience or 0 for p in profiles)
    )
    assert len(years) == 5
    assert all(years[b] / n >= 0.1 for b in ("3-5", "6-10", "11-20")), years

    # GAAP exposure shows up as skills the ranking can use.
    def has_gaap_skill(p: CandidateProfile) -> bool:
        markers = ("gaap", "asc ", "ifrs", "sox", "gasb", "frs 102")
        return any(m in s.lower() for s in p.skills for m in markers)

    assert share(has_gaap_skill) >= 0.4


def test_qualifications_follow_the_candidate_s_country(
    candidates: list[dict[str, Any]], profiles: list[CandidateProfile]
):
    """Nobody in London holds a US CPA: the seed has fully qualified accountants
    from the UK, Ireland, Canada and Australia, each stored as what they hold."""
    tax = taxonomy.load()
    cpa_level = set(tax.acceptable("cpa_us", accept_equivalents=True))
    where: dict[str, set[str]] = {}
    for c, p in zip(candidates, profiles, strict=True):
        for q in p.qualifications:
            if q.canonical in cpa_level:
                where.setdefault(q.jurisdiction or "", set()).add(q.canonical)
            # No ambiguous "CPA" or "CA" is left in the seed.
            assert q.canonical is None or not tax.variants(q.canonical), c["id"]
        if tax.jurisdiction_for_timezone(p.timezone) != "US":
            assert "cpa_us" not in p.certifications, c["id"]
    assert where["UK"] >= {"aca_icaew", "ca_icas", "acca"}
    assert where["Ireland"] == {"ca_ireland"}
    assert where["Canada"] == {"cpa_canada"}
    assert "ca_anz" in where["Australia / New Zealand"]


def test_equivalents_widen_a_cpa_role_and_a_us_licence_does_not(
    candidates: list[dict[str, Any]], profiles: list[CandidateProfile], roles: list[dict[str, Any]]
):
    by_title = {r["title"]: RoleRequirements.model_validate(r["requirements"]) for r in roles}

    def holders(req: RoleRequirements) -> set[str]:
        """What the candidates who meet the role's qualification requirement hold for it."""
        [wanted] = req.required_qualifications
        return {
            check.held
            for p in profiles
            if (check := matching.check_qualification(p, wanted)).passed and check.held is not None
        }

    open_role, us_only = by_title["Controller"], by_title["Tax Manager"]
    assert open_role.required_qualifications[0].accept_equivalents
    assert not open_role.required_qualifications[0].equivalents_stated
    assert holders(open_role) >= {"cpa_us", "cpa_canada", "aca_icaew", "ca_ireland", "ca_anz", "acca"}
    assert not us_only.required_qualifications[0].accept_equivalents
    assert holders(us_only) == {"cpa_us"}
    assert "cpa_us" in holders(by_title["Financial Accountant (IFRS)"])  # and the other way round
    assert len(candidates) == len(profiles)


def test_roles_are_well_formed(roles: list[dict[str, Any]]):
    assert len(roles) >= 5
    ids = [r["id"] for r in roles]
    assert len(set(ids)) == len(ids)
    for r in roles:
        req = RoleRequirements.model_validate(r["requirements"])
        assert req.timezone
        assert req.must_haves
        assert r["description"].strip()
        assert set(r["requirements"]) == set(RoleRequirements.model_fields) - {"starts_on"}, r["id"]


def _passes_hard_filters(
    profile: CandidateProfile, days: int | None, active: bool, req: RoleRequirements, starts_in: int | None
) -> bool:
    """The README shortlist query's WHERE clause, in Python."""
    if not active or not fixtures.passes_hard_filters(profile, req):
        return False
    return starts_in is None or (days is not None and days <= starts_in)


def test_each_role_selects_a_strict_subset(
    candidates: list[dict[str, Any]], profiles: list[CandidateProfile], roles: list[dict[str, Any]]
):
    n = len(candidates)
    counts: dict[str, int] = {}
    for r in roles:
        req = RoleRequirements.model_validate(r["requirements"])
        counts[r["title"]] = sum(
            _passes_hard_filters(p, c["available_in_days"], c["status"] == "active", req, r.get("starts_in_days"))
            for c, p in zip(candidates, profiles, strict=True)
        )
    for title, k in counts.items():
        assert 1 <= k <= 0.9 * n, f"{title}: {k} of {n} candidates pass the hard filters"
    # Different roles must land on different shortlists.
    assert len(set(counts.values())) >= 4, counts


def test_rendered_sql_matches_json():
    stale = [path.name for path, content in render.render_all().items() if path.read_text(encoding="utf-8") != content]
    assert not stale, f"run `make seed-render`; stale: {stale}"
