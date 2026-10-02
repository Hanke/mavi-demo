"""The JD parser against the sample job descriptions in infra/fixtures.

Four things are checked. A correct answer from the model comes back field for
field as the expected requirements, with the must-haves of every sample JD
spelled out below. The requirements schema cleans up or refuses what a model
can get wrong. Every must-have field has a hard-filter column on both sides
of the shortlist query. And the key-free fake finds the same must-haves
wherever a JD uses headed bullet lists. The model is a ScriptedProvider (or
the fake), so nothing here calls an API; `make eval` scores a real model on
the same fixtures.
"""

from __future__ import annotations

import json
import re
from datetime import date
from pathlib import Path
from typing import Any

import pytest
from pydantic import ValidationError

from app import extract, fixtures, llm, matching
from app.extract import JDExtraction
from app.fake import FakeProvider
from app.llm import InvalidOutputError, ScriptedProvider
from app.schemas import HARD_FILTER_COLUMNS, CandidateProfile, RoleRequirements
from app.seedgen import render

SLUGS = fixtures.jd_slugs()
MIGRATIONS = Path(__file__).resolve().parents[2] / "infra" / "db" / "migrations"

# slug -> (required_certifications, required_software, min_years_experience, starts_on, timezone)
MUST_HAVES: dict[str, tuple[list[str], list[str], int | None, date | None, str | None]] = {
    "bookkeeper_part_time_remote": (["quickbooks_proadvisor"], ["quickbooks", "gusto"], 3, None, "America/Los_Angeles"),
    "controller_manufacturing": ([], ["sap"], 10, None, "America/New_York"),
    "financial_controller_cpa_or_equivalent": (["cpa_us"], ["netsuite", "excel"], 8, None, "America/New_York"),
    "fpa_manager_london": (["acca"], ["anaplan", "excel"], 6, None, "Europe/London"),
    "payroll_specialist_remote": ([], ["adp", "excel"], 3, date(2027, 1, 5), "America/New_York"),
    "senior_accountant_strict": (
        ["cpa_us"],
        ["netsuite", "blackline", "excel"],
        5,
        date(2026, 11, 2),
        "America/Los_Angeles",
    ),
    "vague_finance_generalist": ([], [], None, None, None),
}


def _answer(fixture: fixtures.JDFixture) -> dict[str, Any]:
    return json.loads(json.dumps({"company": fixture.raw["company"], "requirements": fixture.raw["requirements"]}))


def _parse(answer: dict[str, Any], text: str) -> JDExtraction:
    return extract.parse_jd(ScriptedProvider([json.dumps(answer)]), text)


def _requirements(**fields: Any) -> RoleRequirements:
    """What the schema makes of an answer, before it is checked against any JD text."""
    return JDExtraction.model_validate({"company": None, "requirements": fields}).requirements


# --- the sample JDs, expected must-haves asserted -------------------------------


def test_every_sample_jd_has_its_must_haves_listed_here():
    assert sorted(MUST_HAVES) == SLUGS


@pytest.mark.parametrize("slug", SLUGS)
def test_sample_jd_parses_to_the_expected_requirements(slug: str):
    fixture = fixtures.load_jd(slug)
    out = _parse(_answer(fixture), fixture.text)
    # The output is the Pydantic schema, and survives a round trip through it.
    assert JDExtraction.model_validate(out.model_dump(mode="json")) == out
    assert out.company == fixture.company
    req = out.requirements
    certifications, software, years, starts_on, timezone = MUST_HAVES[slug]
    assert req.required_certifications == certifications
    assert [q.canonical for q in req.required_qualifications] == certifications
    assert req.required_software == software
    assert req.min_years_experience == years
    assert req.starts_on == starts_on
    assert req.timezone == timezone
    assert req.must_haves == fixture.expected.must_haves
    # Only the part-time bookkeeper JD puts a number on its hours; none on overlap.
    assert req.hours_per_week == (15 if slug == "bookkeeper_part_time_remote" else None)
    assert req.min_overlap_hours is None
    assert req == fixture.expected


def test_must_haves_and_nice_to_haves_of_a_sample_jd():
    """One fixture spelled out, so both tiers are visible here."""
    fixture = fixtures.load_jd("payroll_specialist_remote")
    req = _parse(_answer(fixture), fixture.text).requirements
    assert req.must_haves == [
        "At least 3 years of end-to-end payroll processing on ADP Workforce Now",
        "Multi-state payroll tax experience (we are in CA, WA, OR, AZ, CO, TX, FL, GA and NC)",
        "Experience with a workforce of 1,000 or more hourly employees",
        "Strong Excel skills for reconciliations and audits",
        "Availability during Eastern Time business hours (8am to 5pm ET), from anywhere in the US",
    ]
    assert (req.required_certifications, req.required_software, req.min_years_experience) == ([], ["adp", "excel"], 3)
    # A certification the JD only prefers is structured too, but never a filter.
    assert req.nice_to_haves[0] == "CPP or FPC certification"
    assert req.preferred_certifications == ["cpp", "fpc"]
    assert (req.preferred_software, req.other_preferred_software) == (["workday"], ["UKG"])


# --- the requirements schema ----------------------------------------------------


def test_the_model_is_sent_the_schema_with_both_tiers():
    provider = ScriptedProvider([json.dumps(_answer(fixtures.load_jd("senior_accountant_strict")))])
    extract.parse_jd(provider, fixtures.load_jd("senior_accountant_strict").text)
    system, _, schema = provider.calls[0]
    assert schema == llm.output_schema(JDExtraction)
    properties = schema["$defs"]["RoleRequirements"]["properties"]
    for field in ("required_certifications", "preferred_certifications"):
        assert "cpa_us" in properties[field]["items"]["enum"]
    for field in ("required_software", "preferred_software"):
        assert "netsuite" in properties[field]["items"]["enum"]
    assert set(HARD_FILTER_COLUMNS) <= set(properties)
    assert "min_years_experience" in system
    assert "preferred_software" in system


def test_names_the_model_used_become_ids_and_unknown_ones_are_kept_as_text():
    req = _requirements(
        required_software=["NetSuite", "Dext"],
        preferred_software=["QBO", "Toast"],
        preferred_certifications=["Certified Payroll Professional", "Chartered Tax Adviser"],
    )
    assert (req.required_software, req.other_required_software) == (["netsuite"], ["Dext"])
    assert (req.preferred_software, req.other_preferred_software) == (["quickbooks"], ["Toast"])
    assert (req.preferred_certifications, req.other_preferred_certifications) == (["cpp"], ["Chartered Tax Adviser"])


def test_nothing_is_both_required_and_preferred():
    req = _requirements(
        required_certifications=["cpa_us"],
        required_software=["netsuite", "excel"],
        preferred_certifications=["cpa_us", "cma"],
        preferred_software=["excel", "blackline"],
    )
    assert req.preferred_certifications == ["cma"]
    assert req.preferred_software == ["blackline"]


def test_a_preferred_cpa_is_the_one_where_the_role_is():
    assert _requirements(preferred_certifications=["CPA"], timezone="America/Chicago").preferred_certifications == [
        "cpa_us"
    ]
    assert _requirements(preferred_certifications=["CPA"], timezone="America/Toronto").preferred_certifications == [
        "cpa_canada"
    ]
    assert _requirements(preferred_certifications=["CPA"]).preferred_certifications == ["cpa"]
    # One that is already specific stays what it is.
    assert _requirements(preferred_certifications=["acca"], timezone="America/Chicago").preferred_certifications == [
        "acca"
    ]


@pytest.mark.parametrize("years", [-1, 71, "five"])
def test_an_impossible_minimum_is_never_returned(years: object):
    bad = json.dumps({"company": None, "requirements": {"min_years_experience": years}})
    provider = ScriptedProvider([bad, bad])
    with pytest.raises(InvalidOutputError, match="min_years_experience"):
        extract.parse_jd(provider, "jd")
    assert "requirements.min_years_experience" in provider.calls[1][1]  # the retry names the field


def test_an_empty_answer_is_a_role_with_no_hard_filters():
    req = _requirements()
    assert req == RoleRequirements()
    assert all(not getattr(req, field) for field in HARD_FILTER_COLUMNS)
    assert _requirements(min_years_experience=0).min_years_experience is None  # "no experience needed"
    assert _requirements(min_overlap_hours=0, hours_per_week=0) == RoleRequirements()
    assert set(RoleRequirements.model_fields) == set(req.model_dump())
    with pytest.raises(ValidationError):
        RoleRequirements.model_validate({"minimum_years": 5})


# --- nothing the JD does not say becomes a filter -----------------------------------


@pytest.mark.parametrize("slug", SLUGS)
def test_requirements_the_jd_does_not_state_are_dropped(slug: str, caplog: pytest.LogCaptureFixture):
    fixture = fixtures.load_jd(slug)
    answer = _answer(fixture)
    req = answer["requirements"]
    req["required_certifications"] = [*req["required_certifications"], "pmp"]
    req["required_qualifications"] = [
        *req["required_qualifications"],
        {"name_as_written": "CISA", "canonical": "cisa", "quote": "CISA required"},
    ]
    req["required_software"] = [*req["required_software"], "tableau"]
    req["other_required_software"] = [*req["other_required_software"], "Kyriba"]
    req["preferred_certifications"] = [*req["preferred_certifications"], "cfe"]
    req["preferred_software"] = [*req["preferred_software"], "snowflake"]
    req["min_years_experience"] = 17
    with caplog.at_level("WARNING", logger="app.extract"):
        out = _parse(answer, fixture.text).requirements
    assert out == fixture.expected.model_copy(update={"min_years_experience": None})
    for invented in ("pmp", "CISA", "tableau", "Kyriba", "cfe", "snowflake", "17"):
        assert invented in caplog.text


def test_a_minimum_the_jd_writes_in_words_is_kept_and_an_invented_quote_is_replaced():
    text = "Accountant\n\nRequirements\n- ACCA qualified.\n- A minimum of five years in practice.\n"
    answer = {
        "company": None,
        "requirements": {
            "required_qualifications": [{"name_as_written": "ACCA", "canonical": "acca", "quote": "Must hold ACCA"}],
            "min_years_experience": 5,
        },
    }
    req = _parse(answer, text).requirements
    assert req.min_years_experience == 5
    assert req.required_certifications == ["acca"]
    assert req.required_qualifications[0].quote == "ACCA qualified."
    # "$700 million" and "2.5 years" do not ground a minimum of 70 or 5.
    for years, jd in ((70, "about $700 million in sales"), (5, "2.5 years of runway")):
        invented = {"company": None, "requirements": {"min_years_experience": years}}
        assert _parse(invented, jd).requirements == RoleRequirements()


def test_hours_are_kept_only_when_the_jd_gives_the_number():
    text = (
        "Bookkeeper\n\nRequirements\n- About 20 hours a week.\n"
        "- At least four hours of overlap with Eastern time business hours.\n"
    )
    stated = {
        "company": None,
        "requirements": {"hours_per_week": 20, "min_overlap_hours": 4, "timezone": "America/New_York"},
    }
    req = _parse(stated, text).requirements
    assert (req.hours_per_week, req.min_overlap_hours) == (20, 4)
    # A JD that only says "full-time, Eastern hours" has no number to filter on:
    # the employer is asked at intake instead, and a guess is dropped.
    guessed = {"company": None, "requirements": {"hours_per_week": 40, "min_overlap_hours": 8}}
    assert _parse(guessed, "Accountant\n\nFull-time, Eastern time hours.\n").requirements == RoleRequirements()
    # A number the JD writes about something else is not a number of hours.
    elsewhere = "Accountant\n\nWe are a team of 40 with 4 offices. Eastern time business hours.\n"
    assert _parse(
        guessed | {"requirements": {"hours_per_week": 40, "min_overlap_hours": 4}}, elsewhere
    ).requirements == (RoleRequirements())
    # Nor is a number beside the word "hours" that counts something else.
    for field, hours, line in (
        ("min_overlap_hours", 5, "Full-time, standard 9-5 Eastern time hours."),
        ("min_overlap_hours", 8, "Office hours are 8 a.m. to 5 p.m. Central."),
        ("min_overlap_hours", 4, "Respond to client email within 4 hours."),
        ("hours_per_week", 25, "$25 per hour, part-time."),
        ("hours_per_week", 4, "20 hours a week, with 4 hours of overlap with Central time."),
    ):
        invented = {"company": None, "requirements": {field: hours, "timezone": "America/Chicago"}}
        assert getattr(_parse(invented, f"Accountant\n\n{line}\n").requirements, field) is None, (field, line)
    # Each way a JD does write them.
    for field, hours, line in (
        ("hours_per_week", 15, "Roughly 15 to 20 hours a week."),
        ("hours_per_week", 15, "15-20 hrs/week."),
        ("hours_per_week", 30, "About 30 hours per week."),
        ("hours_per_week", 12, "Twelve hours weekly."),
        ("min_overlap_hours", 4, "Overlap of at least 4 hours with Pacific time."),
        ("min_overlap_hours", 3, "3+ hours of daily overlap with our London team."),
    ):
        stated = {"company": None, "requirements": {field: hours, "timezone": "America/Chicago"}}
        assert getattr(_parse(stated, f"Accountant\n\n{line}\n").requirements, field) == hours, (field, line)
    # The fake reads the same two numbers off the text.
    fake = extract.parse_jd(
        FakeProvider(), "Bookkeeper\n\nRequired\n- 15 to 20 hours per week.\n- 3+ hours of overlap with Central time.\n"
    )
    assert (fake.requirements.hours_per_week, fake.requirements.min_overlap_hours) == (15, 3)


@pytest.mark.parametrize(("field", "hours"), [("min_overlap_hours", 9), ("hours_per_week", 81), ("hours_per_week", -1)])
def test_impossible_hours_are_never_returned(field: str, hours: int):
    bad = json.dumps({"company": None, "requirements": {field: hours}})
    with pytest.raises(InvalidOutputError, match=field):
        extract.parse_jd(ScriptedProvider([bad, bad]), "jd")


# --- must-have fields are the SQL hard filters ------------------------------------


def _columns(table: str) -> set[str]:
    """The table's columns according to the migrations: its CREATE TABLE plus any ADD COLUMN since."""
    sql = "\n".join(p.read_text(encoding="utf-8") for p in sorted(MIGRATIONS.glob("*.up.sql")))
    created = re.search(rf"CREATE TABLE {table} \((.*?)\n\);", sql, re.S)
    assert created, table
    columns = set(re.findall(r"^    ([a-z_]+)\s+\S", created.group(1), re.M))
    return columns | set(re.findall(rf"ALTER TABLE {table} ADD COLUMN ([a-z_]+)", sql))


def test_every_must_have_field_is_a_column_on_both_sides_of_the_shortlist_query():
    roles = _columns("roles")
    candidate_side = {table: _columns(table) for table in ("candidate_profiles", "candidate_availability")}
    assert {"title", "requirements"} <= roles  # the parse above worked
    assert {"profile", "embedding"} <= candidate_side["candidate_profiles"]
    for field, columns in HARD_FILTER_COLUMNS.items():
        assert field in RoleRequirements.model_fields
        assert field in roles, f"roles has no {field} column"
        for qualified in columns:
            table, column = qualified.split(".")
            assert column in candidate_side[table], f"{table} has no {column} column"
            # What a resume shows is a field of the parsed profile; what the
            # candidate supplies about their availability is not.
            assert (column in CandidateProfile.model_fields) or table == "candidate_availability"
    # The filters on availability read what the candidate said, never the parser's guess.
    for field in ("starts_on", "timezone", "min_overlap_hours", "hours_per_week"):
        assert all(c.startswith("candidate_availability.") for c in HARD_FILTER_COLUMNS[field]), field


def test_the_must_have_fields_are_exactly_the_structured_requirements():
    """Anything else the parser returns is a preference, context or free text,
    so a new required_* field cannot be added without a column to filter on."""
    required = {f for f in RoleRequirements.model_fields if f.startswith(("required_", "min_"))}
    assert required - {"required_qualifications"} <= set(HARD_FILTER_COLUMNS)
    assert set(HARD_FILTER_COLUMNS) - required == {"starts_on", "timezone", "hours_per_week"}


def test_the_seed_writes_each_must_have_to_its_column():
    insert = render.render_roles(render.load_json(render.ROLES_JSON))
    header = insert[insert.index("INSERT INTO roles") : insert.index("VALUES")]
    for column in HARD_FILTER_COLUMNS:
        assert re.search(rf"\b{column}\b", header), column


def test_minimum_years_is_compared_with_the_profile_as_sql_would():
    role = RoleRequirements(min_years_experience=5)
    for years, passes in ((5, True), (12, True), (4, False), (None, False)):
        result = matching.check_hard_filters(CandidateProfile(years_experience=years), role)
        assert result.passed is passes, years
    assert matching.check_hard_filters(CandidateProfile(years_experience=1), role).explanations == [
        "Has 1 year of experience; this role asks for at least 5"
    ]
    assert matching.check_hard_filters(CandidateProfile(), RoleRequirements()).passed
    # Preferences never filter.
    wish_list = RoleRequirements(preferred_certifications=["cma"], preferred_software=["power_bi"])
    assert matching.check_hard_filters(CandidateProfile(), wish_list).passed


# --- the key-free fake on the same JDs --------------------------------------------


@pytest.mark.parametrize("slug", [s for s in SLUGS if s != "vague_finance_generalist"])
def test_the_fake_finds_the_must_haves_of_a_jd_with_headed_bullets(slug: str):
    fixture = fixtures.load_jd(slug)
    req = extract.parse_jd(FakeProvider(), fixture.text).requirements
    expected = fixture.expected
    assert req.must_haves == expected.must_haves
    assert req.nice_to_haves == expected.nice_to_haves
    assert req.min_years_experience == expected.min_years_experience
    assert set(req.required_software) >= set(expected.required_software)
    assert set(req.preferred_software) >= set(expected.preferred_software)
    assert req.preferred_certifications == expected.preferred_certifications
    assert not set(req.preferred_software) & set(req.required_software)


def test_the_fake_guesses_nothing_from_a_jd_written_as_prose():
    fixture = fixtures.load_jd("vague_finance_generalist")
    req = extract.parse_jd(FakeProvider(), fixture.text).requirements
    assert (req.required_certifications, req.required_software, req.min_years_experience) == ([], [], None)
    assert (req.preferred_certifications, req.preferred_software) == ([], [])
