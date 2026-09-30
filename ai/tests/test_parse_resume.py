"""The resume parser against the sample resumes in infra/fixtures.

Three things are checked for every fixture. A correct answer from the model
comes back field for field as the expected profile. An answer padded with
things the resume does not say comes back without them. And a resume that
says very little produces nulls and empty lists, not guesses. The model is a
ScriptedProvider (or the key-free fake), so nothing here calls an API;
`make eval` scores a real model on the same fixtures.
"""

from __future__ import annotations

import json
from typing import Any

import pytest

from app import extract, fixtures, grounding
from app.extract import ResumeExtraction
from app.fake import FakeProvider
from app.llm import ScriptedProvider
from app.schemas import CandidateProfile, Contact, Position

SLUGS = fixtures.resume_slugs()

SPARSE_RESUME = """Sam Carter

Bookkeeper. I keep small-business books tidy and on time, and I am comfortable
picking up whatever system a client already uses.
"""


def _answer(fixture: fixtures.ResumeFixture) -> dict[str, Any]:
    return json.loads(json.dumps({"contact": fixture.raw["contact"], "profile": fixture.raw["profile"]}))


def _parse(answer: dict[str, Any], text: str) -> ResumeExtraction:
    return extract.parse_resume(ScriptedProvider([json.dumps(answer)]), text, fixtures.AS_OF)


# --- the sample resumes, expected fields asserted ------------------------------


@pytest.mark.parametrize("slug", SLUGS)
def test_sample_resume_parses_to_the_expected_profile(slug: str):
    fixture = fixtures.load_resume(slug)
    out = _parse(_answer(fixture), fixture.text)
    # The output is the Pydantic schema, and survives a round trip through it.
    assert ResumeExtraction.model_validate(out.model_dump(mode="json")) == out
    expected = fixture.expected
    assert out.contact == fixture.contact
    assert out.profile.years_experience == expected.years_experience
    assert out.profile.positions == expected.positions
    assert out.profile.certifications == expected.certifications
    assert out.profile.other_certifications == expected.other_certifications
    assert out.profile.software == expected.software
    assert out.profile.other_software == expected.other_software
    assert out.profile.gaap_exposure == expected.gaap_exposure
    assert out.profile.industries == expected.industries
    assert out.profile == expected


def test_expected_roles_and_years_of_a_sample_resume():
    """One fixture spelled out, so the shape of the five scoped fields is visible here."""
    fixture = fixtures.load_resume("senior_accountant_cpa_netsuite")
    profile = _parse(_answer(fixture), fixture.text).profile
    assert profile.years_experience == 7
    assert [(p.title, p.employer, p.start_year, p.end_year, p.current) for p in profile.positions] == [
        ("Senior Accountant", "Lumen Grid Software", 2023, None, True),
        ("Accountant", "Harborline Analytics", 2020, 2023, False),
        ("Audit Associate", "Keller, Tan & Associates LLP", 2019, 2020, False),
    ]
    assert profile.certifications == ["cpa_us"]
    assert profile.software == ["netsuite", "blackline", "expensify", "stripe", "excel", "google_sheets"]
    assert profile.gaap_exposure == ["ASC 606", "ASC 350-40"]
    assert profile.industries == ["saas", "accounting_services"]


# --- missing information comes back empty, not invented ------------------------


@pytest.mark.parametrize("slug", SLUGS)
def test_values_the_resume_does_not_carry_are_removed(slug: str, caplog: pytest.LogCaptureFixture):
    """The model pads a correct answer with a certification, tools, a standard
    and a job that appear nowhere in the resume; none of it survives."""
    fixture = fixtures.load_resume(slug)
    answer = _answer(fixture)
    profile = answer["profile"]
    profile["certifications"].append("cfa")
    profile["other_certifications"].append("Six Sigma Black Belt")
    profile["software"].append("hyperion")
    profile["other_software"].append("Kyriba")
    profile["gaap_exposure"].append("ASC 944")
    profile["positions"].append(
        {"title": "Chief Accounting Officer", "employer": "Globex", "start_year": 1999, "end_year": 2001}
    )
    with caplog.at_level("WARNING", logger="app.extract"):
        out = _parse(answer, fixture.text)
    assert out.profile == fixture.expected
    assert out.contact == fixture.contact
    assert "Chief Accounting Officer" in caplog.text
    assert "cfa" in caplog.text


def test_invented_contact_details_and_years_become_null():
    fixture = fixtures.load_resume("staff_accountant_early_career")
    answer = _answer(fixture)
    answer["contact"] |= {"email": "maya@gmail.com", "phone": "+1 303 555 0100", "full_name": "Maya L. Lindqvist-Smith"}
    answer["profile"]["positions"][0] |= {"employer": "Deloitte", "start_year": 2019}
    out = _parse(answer, fixture.text)
    assert out.contact == Contact(location=fixture.contact.location)
    first = out.profile.positions[0]
    assert (first.title, first.employer, first.start_year) == ("Staff Accountant", None, None)
    assert out.profile.positions[1:] == fixture.expected.positions[1:]


def test_a_reformatted_phone_number_or_name_is_not_treated_as_invented():
    fixture = fixtures.load_resume("messy_ap_specialist")  # the name is all caps in the text
    answer = _answer(fixture)
    answer["contact"]["phone"] = "(214) 555-0177"
    out = _parse(answer, fixture.text)
    assert out.contact.full_name == "Jordan Blake"
    assert out.contact.phone == "(214) 555-0177"


def test_sparse_resume_comes_back_empty_even_when_the_model_guesses():
    guess: dict[str, Any] = {
        "contact": {"full_name": "Sam Carter", "email": "sam.carter@example.com", "phone": "", "location": None},
        "profile": {
            "headline": "Bookkeeper",
            "years_experience": 5,
            "positions": [{"title": "Bookkeeper", "employer": "Self-employed", "start_year": 2021, "current": True}],
            "certifications": ["cpb"],
            "software": ["QuickBooks Online", "excel"],
            "industries": [],
            "gaap_exposure": ["US GAAP"],
        },
    }
    out = _parse(guess, SPARSE_RESUME)
    assert out.contact == Contact(full_name="Sam Carter")
    assert out.profile == CandidateProfile(
        headline="Bookkeeper",
        positions=[Position(title="Bookkeeper")],
    )
    assert out.profile.availability == "unknown"


def test_sparse_resume_with_the_fake_provider_is_empty():
    out = extract.parse_resume(FakeProvider(), SPARSE_RESUME, fixtures.AS_OF)
    assert out.contact == Contact(full_name="Sam Carter")
    profile = out.profile
    assert profile.years_experience is None
    assert profile.positions == []
    assert profile.certifications == profile.software == profile.industries == profile.gaap_exposure == []
    assert profile.available_from is None
    assert profile.timezone is None


def test_a_day_of_the_month_does_not_ground_a_year():
    assert grounding.year_in_text(2023, "Jan '23 to Aug 2024")
    assert grounding.year_in_text(2023, "06/23 - present")
    assert not grounding.year_in_text(2023, "born 10/23/1990, joined 2019")
    assert not grounding.year_in_text(2030, "joined 2019")


def test_empty_contact_strings_are_null():
    assert Contact.model_validate({"full_name": " ", "email": "", "phone": None}) == Contact()


def test_the_prompt_tells_the_model_not_to_guess():
    provider = ScriptedProvider(['{"contact": {}, "profile": {}}'])
    out = extract.parse_resume(provider, SPARSE_RESUME, fixtures.AS_OF)
    assert out == ResumeExtraction(contact=Contact(), profile=CandidateProfile())
    system, _, schema = provider.calls[0]
    assert "never guess" in system
    properties = schema["$defs"]["CandidateProfile"]["properties"]
    assert {"positions", "certifications", "software", "gaap_exposure", "industries"} <= set(properties)


# --- position validation -------------------------------------------------------


def test_position_years_must_be_ordered_and_a_current_role_has_no_end():
    assert Position(title="Controller", start_year=2020, end_year=2026, current=True).end_year is None
    with pytest.raises(ValueError, match="before start_year"):
        Position(title="Controller", start_year=2020, end_year=2018)


@pytest.mark.parametrize("slug", SLUGS)
def test_fake_provider_reads_the_named_standards(slug: str):
    fixture = fixtures.load_resume(slug)
    out = extract.parse_resume(FakeProvider(), fixture.text, fixtures.AS_OF)
    assert out.profile.gaap_exposure == fixture.expected.gaap_exposure
    assert out.contact.email == fixture.contact.email
