"""Qualifications across jurisdictions: a UK candidate holds an ACA, not a CPA.

The parser records the qualification that is actually held (never converted),
and the hard filter decides equivalence from the groups in
infra/taxonomy.json. The first block is the ticket's acceptance criteria run
on the fixtures; the rest pins down the rules behind them.
"""

from __future__ import annotations

import json
from typing import Any

import pytest

from app import extract, fixtures, matching, qualifications, taxonomy
from app.fake import FakeProvider
from app.llm import ScriptedProvider
from app.schemas import CandidateProfile, Qualification, RequiredQualification, RoleRequirements
from app.verification import NoRegistry

OR_EQUIVALENT = "financial_controller_cpa_or_equivalent"
US_LICENCE = "senior_accountant_strict"


def _profile(slug: str) -> CandidateProfile:
    return fixtures.load_resume(slug).expected


def _check(resume: str, jd: str) -> matching.HardFilterResult:
    return matching.check_hard_filters(_profile(resume), fixtures.load_jd(jd).expected)


def _parsed(slug: str) -> CandidateProfile:
    """The fixture's expected answer put through the real parser path."""
    fixture = fixtures.load_resume(slug)
    answer = json.dumps({"contact": fixture.raw["contact"], "profile": fixture.raw["profile"]})
    return extract.parse_resume(ScriptedProvider([answer]), fixture.text, fixtures.AS_OF).profile


# --- acceptance criteria -------------------------------------------------------


def test_aca_is_stored_as_aca_and_passes_cpa_or_equivalent():
    profile = _parsed("financial_controller_aca_uk")
    [aca] = profile.qualifications
    assert (aca.name_as_written, aca.canonical, aca.issuing_body, aca.jurisdiction) == (
        "ACA",
        "aca_icaew",
        "ICAEW",
        "UK",
    )
    assert (aca.status, aca.year_obtained, aca.quote) == ("qualified", 2019, "ACA, ICAEW, admitted 2019")
    assert profile.certifications == ["aca_icaew"]

    req = fixtures.load_jd(OR_EQUIVALENT).expected
    [wanted] = req.required_qualifications
    assert (wanted.canonical, wanted.accept_equivalents, wanted.equivalents_stated) == ("cpa_us", True, True)
    result = matching.check_hard_filters(profile, req)
    assert result.passed
    assert result.explanations == ["Holds ACA (ICAEW, UK), equivalent to the US CPA this role asks for"]


def test_the_same_candidate_fails_a_role_that_needs_the_us_licence():
    req = fixtures.load_jd(US_LICENCE).expected
    [wanted] = req.required_qualifications
    assert (wanted.canonical, wanted.accept_equivalents, wanted.equivalents_stated) == ("cpa_us", False, True)
    result = matching.check_hard_filters(_profile("financial_controller_aca_uk"), req)
    # She has every product the role requires: the licence is the only reason.
    assert not result.passed
    assert result.missing_software == []
    assert result.explanations == [
        "Holds ACA (ICAEW, UK), the same level as a US CPA, but this role requires the US CPA itself"
    ]
    assert _check("senior_accountant_cpa_netsuite", US_LICENCE).passed


def test_part_qualified_acca_does_not_pass_a_fully_qualified_requirement():
    profile = _parsed("assistant_accountant_part_qualified")
    acca = next(q for q in profile.qualifications if q.canonical == "acca")
    assert acca.status == "part_qualified"
    assert "acca" not in profile.certifications
    for requirement in (
        RoleRequirements(required_certifications=["acca"]),
        RoleRequirements(
            required_qualifications=[RequiredQualification(name_as_written="ACCA", accept_equivalents=False)]
        ),
        fixtures.load_jd(OR_EQUIVALENT).expected,
    ):
        [check] = matching.check_hard_filters(profile, requirement).qualifications
        assert not check.passed
        assert check.explanation.startswith("Part-qualified ACCA")


def test_aat_and_cima_do_not_pass_as_cpa_equivalents():
    for slug, held in (("assistant_accountant_part_qualified", "aat"), ("management_accountant_cima", "cima")):
        profile = _profile(slug)
        assert held in profile.certifications  # fully held, and still not enough
        assert held not in taxonomy.load().acceptable("cpa_us", accept_equivalents=True)
        [check] = _check(slug, OR_EQUIVALENT).qualifications
        assert not check.passed, slug
    [cima] = _check("management_accountant_cima", OR_EQUIVALENT).qualifications
    assert "management accounting" in cima.explanation
    assert "not equivalent" in cima.explanation


def test_each_fixture_jd_passes_exactly_the_resumes_it_lists():
    passing = sorted(r.slug for r in fixtures.load_resumes() if _check(r.slug, OR_EQUIVALENT).passed)
    # The US CPA with NetSuite holds the right licence but has 7 of the 8 years asked for.
    assert passing == ["financial_controller_aca_uk", "fpa_manager_london", "fractional_cfo"]
    short = _check("senior_accountant_cpa_netsuite", OR_EQUIVALENT)
    assert all(c.passed for c in short.qualifications)
    assert short.experience_short == "Has 7 years of experience; this role asks for at least 8"


# --- extraction: what is held is never turned into something else ---------------


def test_a_model_that_answers_cpa_for_an_aca_is_overruled():
    profile = CandidateProfile(
        certifications=["cpa"],
        qualifications=[Qualification(name_as_written="ACA", canonical="cpa", issuing_body="ICAEW")],
    )
    assert profile.certifications == ["aca_icaew"]
    assert profile.qualifications[0].canonical == "aca_icaew"


@pytest.mark.parametrize(
    ("name", "body", "where", "timezone", "want"),
    [
        # "CPA" alone is three different qualifications.
        ("CPA", None, None, "America/Chicago", ("cpa_us", "US")),
        ("CPA", None, None, "America/Toronto", ("cpa_canada", "Canada")),
        ("CPA", None, None, "Australia/Sydney", ("cpa_australia", "Australia")),
        ("CPA", "CPA Ontario", None, "America/New_York", ("cpa_canada", "Canada")),  # the body beats the address
        ("CPA", None, "Australia", "Europe/London", ("cpa_australia", "Australia")),  # so does a stated country
        ("CPA", None, None, None, ("cpa", None)),  # cannot be determined: left empty
        ("CPA", None, "Philippines", "Asia/Manila", ("cpa", "Philippines")),  # a CPA the map does not list
        ("US CPA", None, None, "Europe/London", ("cpa_us", "US")),  # already specific
        # Chartered accountants.
        ("ACA", None, None, "Europe/London", ("aca_icaew", "UK")),
        ("ACA", None, None, "Europe/Dublin", ("ca_ireland", "Ireland")),
        ("ACA", "Chartered Accountants Ireland", None, "Europe/London", ("ca_ireland", "Ireland")),
        ("CA", "ICAS", None, None, ("ca_icas", "UK")),
        ("CA", None, None, "Europe/London", ("ca_icas", "UK")),
        ("Chartered Accountant", "ICAEW", None, None, ("aca_icaew", "UK")),
        ("CA", None, None, "Australia/Sydney", ("ca_anz", "Australia / New Zealand")),
        ("CA", None, None, "Asia/Kolkata", ("ca_icai", "India")),
        ("Chartered Accountant", None, None, None, ("ca", None)),
        ("CPA, CA", None, None, None, ("cpa_canada", "Canada")),
        ("ACCA", None, None, "Asia/Manila", ("acca", "UK")),
        ("Series 7", None, None, "America/New_York", (None, None)),
    ],
)
def test_shared_letters_are_settled_by_body_then_country(
    name: str, body: str | None, where: str | None, timezone: str | None, want: tuple[str | None, str | None]
):
    profile = CandidateProfile(
        qualifications=[Qualification(name_as_written=name, issuing_body=body, jurisdiction=where)], timezone=timezone
    )
    [q] = profile.qualifications
    assert (q.canonical, q.jurisdiction) == want
    assert profile.certifications == ([want[0]] if want[0] else [])
    assert profile.other_certifications == ([] if want[0] else [name])


@pytest.mark.parametrize(
    ("name", "quote", "want"),
    [
        ("ACCA", "ACCA finalist", "part_qualified"),
        ("Part-qualified ACCA", None, "part_qualified"),
        ("ACCA", "ACCA: part-qualified, 9 of 13 papers passed", "part_qualified"),
        ("CPA", "CPA candidate: FAR and AUD passed", "in_progress"),
        ("ACA", "Studying for the ACA with ICAEW", "in_progress"),
        ("ACCA", "ACCA member, qualified 2020", "qualified"),
        # The words belong to the qualification they sit next to.
        ("AAT", "AAT qualified (MAAT) and part-qualified ACCA: 9 of 13 papers passed", "qualified"),
        ("ACA", "ACA, ICAEW, 2018; currently studying for the CTA", "qualified"),
    ],
)
def test_a_qualification_that_is_not_finished_is_not_held(name: str, quote: str | None, want: str):
    profile = CandidateProfile(
        certifications=[name] if taxonomy.load().resolve("certifications", name) else [],
        qualifications=[Qualification(name_as_written=name, quote=quote)],
        timezone="Europe/London",
    )
    assert profile.qualifications[0].status == want
    assert bool(profile.certifications) == (want == "qualified")


def test_status_is_only_ever_downgraded():
    assert qualifications.status_from("ACCA", "ACCA member", "in_progress") == "in_progress"


def test_grounding_drops_a_qualification_the_resume_does_not_name():
    fixture = fixtures.load_resume("financial_controller_aca_uk")
    answer: dict[str, Any] = json.loads(
        json.dumps({"contact": fixture.raw["contact"], "profile": fixture.raw["profile"]})
    )
    answer["profile"]["qualifications"].append({"name_as_written": "CPA", "canonical": "cpa_us", "jurisdiction": "US"})
    answer["profile"]["qualifications"][0] |= {"quote": "ACA (ICAEW), first-time passes", "year_obtained": 2011}
    out = extract.parse_resume(ScriptedProvider([json.dumps(answer)]), fixture.text, fixtures.AS_OF).profile
    assert out.certifications == ["aca_icaew"]
    [aca] = out.qualifications
    # An invented quote is replaced by the line that does name it; an invented year is cleared.
    assert aca.quote == "Hannah Whitaker ACA"
    assert aca.year_obtained is None


@pytest.mark.parametrize(
    ("slug", "held", "not_held"),
    [
        ("financial_controller_aca_uk", ["aca_icaew"], []),
        ("assistant_accountant_part_qualified", ["aat"], ["acca"]),
        ("management_accountant_cima", ["cima", "cgma"], []),
        ("internal_auditor_toronto", ["cpa_canada", "cia"], []),
        ("staff_accountant_early_career", [], ["cpa_us"]),
    ],
)
def test_the_fake_provider_reads_qualifications_off_the_page(slug: str, held: list[str], not_held: list[str]):
    fixture = fixtures.load_resume(slug)
    profile = extract.parse_resume(FakeProvider(), fixture.text, fixtures.AS_OF).profile
    assert profile.certifications == held
    assert [q.canonical for q in profile.qualifications if q.status != "qualified"] == not_held


# --- the requirement side --------------------------------------------------------


def test_a_jd_that_does_not_say_defaults_to_accepting_equivalents():
    req = fixtures.load_jd("fpa_manager_london").expected
    [wanted] = req.required_qualifications
    assert wanted.accept_equivalents
    assert not wanted.equivalents_stated  # so it is shown to the employer at intake
    # A role stored as a bare id list, with no record at all, behaves the same.
    bare = RoleRequirements(required_certifications=["cpa"])
    assert matching.check_hard_filters(_profile("financial_controller_aca_uk"), bare).passed


def test_which_cpa_a_jd_means_comes_from_its_words_then_its_location():
    def canonical(quote: str, timezone: str | None = None) -> str | None:
        req = RoleRequirements(
            required_qualifications=[RequiredQualification(name_as_written="CPA", quote=quote)], timezone=timezone
        )
        assert req.required_certifications == [req.required_qualifications[0].canonical]
        return req.required_qualifications[0].canonical

    assert canonical("Active CPA license (any US state)") == "cpa_us"
    assert canonical("CPA or equivalent", "America/Toronto") == "cpa_canada"
    assert canonical("CPA or equivalent") == "cpa"


@pytest.mark.parametrize("slug", [OR_EQUIVALENT, US_LICENCE])
def test_the_fake_provider_reads_whether_equivalents_are_accepted(slug: str):
    fixture = fixtures.load_jd(slug)
    got = extract.parse_jd(FakeProvider(), fixture.text).requirements.required_qualifications
    [want] = fixture.expected.required_qualifications
    [q] = got  # the ACA, ACCA and CA in brackets are examples, not requirements
    assert (q.accept_equivalents, q.equivalents_stated) == (want.accept_equivalents, want.equivalents_stated)
    # The fake does not work out where a role is, so "CPA" may stay the ambiguous id.
    assert q.canonical in (want.canonical, "cpa")


# --- matching ------------------------------------------------------------------


def test_an_undetermined_cpa_is_not_assumed_to_be_an_equivalent():
    unknown = CandidateProfile(qualifications=[Qualification(name_as_written="CPA")])
    assert unknown.certifications == ["cpa"]
    us_role = fixtures.load_jd(OR_EQUIVALENT).expected
    [check] = matching.check_hard_filters(unknown, us_role).qualifications
    assert not check.passed
    assert "does not say which body or country" in check.explanation
    # A role that asks for "a CPA" without saying which takes any of them.
    assert matching.check_hard_filters(unknown, RoleRequirements(required_certifications=["cpa"])).passed


def test_equivalence_runs_both_ways_and_stays_inside_the_group():
    london_role = fixtures.load_jd("fpa_manager_london").expected  # ACCA, equivalents by default
    us_cpa = CandidateProfile(
        qualifications=[Qualification(name_as_written="CPA", jurisdiction="US")],
        software=["anaplan", "excel"],
        years_experience=8,
    )
    result = matching.check_hard_filters(us_cpa, london_role)
    assert result.passed
    assert result.explanations[0].endswith("equivalent to the ACCA this role asks for")
    cima_role = RoleRequirements(required_qualifications=[RequiredQualification(name_as_written="CIMA")])
    assert matching.check_hard_filters(_profile("management_accountant_cima"), cima_role).passed
    assert not matching.check_hard_filters(_profile("financial_controller_aca_uk"), cima_role).passed


def test_a_candidate_with_nothing_relevant_gets_a_plain_reason():
    [check] = _check("messy_ap_specialist", OR_EQUIVALENT).qualifications
    assert check.explanation == "No US CPA or equivalent on the resume"


# --- out of scope, stubbed -------------------------------------------------------


def test_registry_verification_is_a_stub():
    [aca] = _profile("financial_controller_aca_uk").qualifications
    assert NoRegistry().verify("Hannah Whitaker", aca).outcome == "unverified"
