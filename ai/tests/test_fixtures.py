"""The hand-written parser fixtures in infra/fixtures (loaded by app.fixtures).

These check the fixtures themselves, so a parser test that fails against them
is a parser problem and not a typo in an expected file: every expected output
is a valid, canonical CandidateProfile / RoleRequirements; the text really
carries every hard-filter value the expected output claims; the PDF is the
text; and the set covers what the fixtures exist for (a messy resume, a vague
JD, a strict JD, values outside the taxonomy, non-US time zones).

Only the slugs are read at import. Each fixture is loaded inside its own
test, so a malformed expected file fails that fixture's tests instead of
aborting the whole run at collection.
"""

from __future__ import annotations

import zoneinfo

import pytest

from app import fixtures, taxonomy
from app.extract import ResumeExtraction, ground_resume
from app.fixtures import JDFixture, ResumeFixture
from app.grounding import mentions
from app.schemas import CandidateProfile, RoleRequirements

RESUME_IDS = fixtures.resume_slugs()
JD_IDS = fixtures.jd_slugs()


@pytest.fixture(params=RESUME_IDS)
def resume(request: pytest.FixtureRequest) -> ResumeFixture:
    return fixtures.load_resume(request.param)


@pytest.fixture(params=JD_IDS)
def jd(request: pytest.FixtureRequest) -> JDFixture:
    return fixtures.load_jd(request.param)


@pytest.fixture(scope="module")
def resumes() -> list[ResumeFixture]:
    return fixtures.load_resumes()


@pytest.fixture(scope="module")
def jds() -> list[JDFixture]:
    return fixtures.load_jds()


def _in_text(fragment: str, text: str) -> bool:
    return fragment.lower() in " ".join(text.split()).lower()


def test_fixture_set_size():
    assert 8 <= len(RESUME_IDS) <= 10
    assert 4 <= len(JD_IDS) <= 6


def test_every_file_belongs_to_a_fixture():
    expected_resume_files = {f"{s}{ext}" for s in RESUME_IDS for ext in (".txt", ".pdf", fixtures.EXPECTED_SUFFIX)}
    assert {p.name for p in fixtures.RESUMES_DIR.iterdir() if not p.name.startswith(".")} == expected_resume_files
    expected_jd_files = {f"{s}{ext}" for s in JD_IDS for ext in (".txt", fixtures.EXPECTED_SUFFIX)}
    assert {p.name for p in fixtures.JDS_DIR.iterdir() if not p.name.startswith(".")} == expected_jd_files


# --- resumes -----------------------------------------------------------------


def test_resume_expected_is_canonical(resume: ResumeFixture):
    """The committed JSON is exactly what the parser must return: every field
    present, ids already canonical, nothing the validator would rewrite."""
    profile = resume.raw["profile"]
    assert set(profile) == set(CandidateProfile.model_fields), resume.slug
    assert resume.expected.model_dump(mode="json") == profile, resume.slug
    assert set(resume.raw) == {"$comment", "contact", "profile"}
    assert resume.comment
    assert resume.expected.headline
    assert resume.expected.years_experience is not None
    assert resume.expected.timezone in zoneinfo.available_timezones()
    assert resume.expected.skills
    assert resume.expected.languages


def test_resume_text_carries_the_expected_values(resume: ResumeFixture):
    tax = taxonomy.load()
    text = resume.text
    assert resume.contact.full_name
    assert resume.contact.full_name.lower() in text.lower()
    assert resume.contact.email
    assert resume.contact.email in text
    assert resume.contact.phone is None or resume.contact.phone in text
    terms = [tax.term("certifications", c) for c in resume.expected.certifications]
    terms += [tax.term("software", s) for s in resume.expected.software]
    missing = [t.id for t in terms if not mentions(text, t)]
    assert not missing, f"{resume.slug}: not named in the resume: {missing}"
    for other in resume.expected.other_software + resume.expected.other_certifications:
        assert _in_text(other, text), f"{resume.slug}: {other!r} not in the resume"
        for kind in ("software", "certifications"):
            assert tax.resolve(kind, other) is None, f"{resume.slug}: {other!r} is in the taxonomy; use its id"
    if resume.expected.available_from is not None:
        assert resume.expected.availability != "unknown"
    assert resume.expected.positions, resume.slug
    assert resume.expected.positions[0].current, f"{resume.slug}: positions are in resume order, current role first"


def test_resume_expected_survives_grounding(resume: ResumeFixture):
    """Nothing in an expected output is absent from its resume: the pass that
    strips invented values from the parser's answer leaves it untouched."""
    expected = ResumeExtraction(contact=resume.contact, profile=resume.expected)
    assert ground_resume(expected, resume.text) == expected, resume.slug


def test_resume_pdf_is_the_text(resume: ResumeFixture):
    rendered = fixtures.render_pdf(resume.text)
    assert rendered.startswith(b"%PDF-1.4")
    assert rendered.endswith(b"%%EOF\n")
    assert resume.pdf_path.read_bytes() == rendered, f"run `make fixtures-render`; {resume.pdf_path.name} is stale"


def test_render_pdf_is_deterministic_and_paginates():
    text = "Name\n" + "\n".join(f"line {i} " + "word " * 30 for i in range(200))
    rendered = fixtures.render_pdf(text)
    assert rendered == fixtures.render_pdf(text)
    assert rendered.count(b"/Type /Page ") > 1
    assert fixtures.render_pdf("").count(b"/Type /Page ") == 1
    with pytest.raises(UnicodeEncodeError):
        fixtures.render_pdf("arrow → outside cp1252")


def test_wrap_keeps_tokens_and_never_raises():
    email = "a.very.long.mailbox.name.that.nobody.would.type@subdomain.of.some.employer.example.com"
    lines = fixtures.render_pdf("Name\n" + "words " * 20 + email)
    assert email.encode() in lines, "a long token must overflow, not be split"
    # An absurd indent used to make textwrap's width negative.
    deep = fixtures.wrap_line(" " * 100 + "hello world " * 10)
    assert len(deep) > 1
    # First line and continuation lines share the same right margin.
    wrapped = fixtures.wrap_line("  - " + "word " * 40)
    assert max(map(len, wrapped)) <= fixtures.MAX_CHARS
    assert len(wrapped[0]) > fixtures.MAX_CHARS - len("word ") - 2


# --- job descriptions --------------------------------------------------------


def test_jd_expected_is_canonical(jd: JDFixture):
    requirements = jd.raw["requirements"]
    assert set(requirements) == set(RoleRequirements.model_fields), jd.slug
    assert jd.expected.model_dump(mode="json") == requirements, jd.slug
    assert set(jd.raw) == {"$comment", "company", "requirements", "hard_filter_matches"}
    assert jd.comment
    assert jd.company
    assert jd.expected.title
    assert jd.expected.must_haves
    assert jd.expected.timezone is None or jd.expected.timezone in zoneinfo.available_timezones()


def test_jd_text_carries_the_expected_values(jd: JDFixture):
    tax = taxonomy.load()
    terms = [tax.term("certifications", c) for c in jd.expected.required_certifications]
    terms += [tax.term("software", s) for s in jd.expected.required_software]
    missing = [t.id for t in terms if not mentions(jd.text, t)]
    assert not missing, f"{jd.slug}: not named in the JD: {missing}"
    # must_haves and nice_to_haves are "in the JD's own words": each is a verbatim fragment.
    for line in jd.expected.must_haves + jd.expected.nice_to_haves:
        assert _in_text(line, jd.text), f"{jd.slug}: {line!r} is not verbatim from the JD"
    assert not set(jd.expected.must_haves) & set(jd.expected.nice_to_haves)
    for other in jd.expected.other_required_software:
        assert _in_text(other, jd.text)
        assert tax.resolve("software", other) is None


def test_jd_hard_filter_matches_are_computed_from_the_resumes(jd: JDFixture, resumes: list[ResumeFixture]):
    """Which fixture resumes pass this JD's certification and software
    containment. Only those two clauses: the shortlist query's start-date
    clause needs the availability date the pipeline derives, so a pipeline
    test applies that on top."""
    passing = sorted(r.slug for r in resumes if fixtures.passes_hard_filters(r.expected, jd.expected))
    assert jd.hard_filter_matches == passing, jd.slug
    assert passing, f"{jd.slug}: no fixture resume passes its hard filters"


# --- the set as a whole ------------------------------------------------------


def test_resume_set_covers_the_brief(resumes: list[ResumeFixture]):
    by_slug = {r.slug: r for r in resumes}
    assert "messy_ap_specialist" in by_slug
    years = [r.expected.years_experience or 0 for r in resumes]
    assert min(years) <= 3, "one resume should be early career"
    assert max(years) >= 15, "one resume should be very senior"
    assert len({r.expected.availability for r in resumes}) >= 4
    assert any(r.expected.available_from for r in resumes), "one resume should state an explicit start date"
    assert len({r.expected.timezone for r in resumes}) >= 5
    assert any(not r.expected.timezone.startswith("America/") for r in resumes if r.expected.timezone)
    assert any(r.expected.other_software for r in resumes), "one resume should name software outside the taxonomy"
    assert any(not r.expected.certifications for r in resumes)
    assert any(len(r.expected.certifications) >= 2 for r in resumes)
    assert any(r.expected.languages != ["en"] for r in resumes)
    assert any(not r.expected.gaap_exposure for r in resumes), "one resume should name no accounting standard"
    assert {"US GAAP", "IFRS"} <= {g for r in resumes for g in r.expected.gaap_exposure}
    assert any(len({p.employer for p in r.expected.positions}) < len(r.expected.positions) for r in resumes)
    assert len({r.contact.email for r in resumes}) == len(resumes)
    assert all((r.contact.email or "").endswith("@example.com") for r in resumes), "no real-looking contact details"


def test_jd_set_covers_the_brief(jds: list[JDFixture]):
    vague = [
        j
        for j in jds
        if not j.expected.required_certifications
        and not j.expected.required_software
        and not j.expected.industries
        and j.expected.timezone is None
    ]
    assert len(vague) == 1, "exactly one JD with no hard filters at all"
    strict = [
        j
        for j in jds
        if j.expected.required_certifications
        and len(j.expected.required_software) >= 2
        and len(j.expected.must_haves) >= 5
        and j.expected.starts_on is not None
    ]
    assert strict, "one JD with certification, software, several must-haves and a start date"
    assert any(j.expected.other_required_software for j in jds), "one JD requiring software outside the taxonomy"
    assert any(j.expected.starts_on for j in jds)
    assert len({j.expected.timezone for j in jds}) >= 3
    # Different JDs must land on different shortlists, and the vague one on everyone.
    assert len({tuple(j.hard_filter_matches) for j in jds}) == len(jds)
    assert vague[0].hard_filter_matches == sorted(RESUME_IDS)
