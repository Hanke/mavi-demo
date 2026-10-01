"""The prompts and output models behind /parse-resume, /parse-jd and /rerank.

Each function takes a Provider and returns a validated model (the parsers
also strip anything the text does not carry, see `ground_resume` and
`ground_jd`); the HTTP layer
in app/main.py only wraps them in request and response types and maps
LLMError to a status code. Everything the model is asked for is described by
a Pydantic model whose schema is sent along with the prompt, so the shapes
here are also the contract with the model.
"""

from __future__ import annotations

import logging
import re
from datetime import date
from typing import Any

from pydantic import BaseModel, ConfigDict, Field

from app import llm, rubric, taxonomy
from app.grounding import in_text, line_with, mentions, year_in_text
from app.llm import Provider
from app.schemas import (
    CandidateProfile,
    Contact,
    Position,
    Qualification,
    RequiredQualification,
    RoleRequirements,
)

log = logging.getLogger(__name__)

RESUME_SYSTEM = """You extract structured data from resumes for a finance and accounting recruiting product.

Read the resume text and fill in the schema. Extract only what the resume states. When it does not state something, return null (or an empty list); never guess, and never fill a gap with something plausible. Rules:
- contact: the candidate's name, email, phone and location exactly as written. Use null for anything absent.
- profile.headline: one line that says what the candidate is, e.g. "Senior Accountant, CPA - NetSuite multi-entity close".
- profile.years_experience: whole years of professional experience from the first professional role to the date given as "today". Exclude internships and study. null if it cannot be worked out.
- profile.positions: every job in the work history, in the order the resume lists them. title and employer as written (employer null if none is named); start_year and end_year as four-digit years, null when the resume gives none. A role that runs to the present has current true and end_year null. When one date range carries two titles ("Staff Accountant, then Accountant"), return one position with the most recent title. Clients served within a self-employed or consulting role are not separate positions. Internships are positions.
- profile.certifications / profile.software / profile.industries: only ids from the enum lists in the schema. Use a candidate's own wording for an item not in the list by putting it in other_certifications / other_software / other_industries instead. Never invent a certification or tool the resume does not name. A certification counts only when it is held: exam progress, "candidate" or "studying for" is not one.
- profile.qualifications: one entry for every professional qualification or certification the resume mentions, including ones not yet held. name_as_written is exactly what the resume says ("ACA", "CPA, CA", "Chartered Accountant"). canonical is the enum id of that qualification itself. Never convert one qualification into another: an ACA is aca_icaew, never cpa, however similar the two are. Letters that several bodies share are ambiguous on their own: "CPA" is cpa_us, cpa_canada or cpa_australia, and "CA" or "ACA" belong to several institutes. Pick the specific id only when the issuing body or the candidate's location settles it; otherwise use the plain id (cpa, ca, aca) and leave jurisdiction null. issuing_body as the resume names it, null if it does not; jurisdiction as a country ("UK", "US", "Australia"). status is qualified only when the qualification is fully held: "part-qualified", "finalist" or some exams passed is part_qualified, and "studying for", "candidate" or "in progress" is in_progress ("ACCA finalist" is not ACCA). year_obtained only if stated. quote is the resume's own words showing it, verbatim.
- profile.gaap_exposure: the accounting frameworks and standards the resume names, each written as the resume writes it, e.g. "US GAAP", "IFRS", "ASC 606", "IFRS 17", "FRS 102". Only ones named in the text; do not infer a framework from the candidate's country or job title. Empty if none is named.
- profile.skills: 6 to 14 short skills the resume evidences, lower case except acronyms, e.g. "month-end close", "ASC 606 revenue recognition".
- profile.languages: ISO 639-1 codes, "en" if nothing else is stated.
- profile.availability: immediate | two_weeks | one_month | unavailable | unknown, from what the resume says; unknown when it says nothing. available_from: an ISO date only when the resume states one.
- profile.timezone: the IANA zone of the candidate's location, e.g. America/Chicago, Europe/London; null if there is no location.

Return only the JSON object."""

JD_SYSTEM = """You extract structured requirements from job descriptions for a finance and accounting recruiting product.

Read the job description and fill in the schema. Rules:
- company: the hiring company's name if stated, otherwise null.
- requirements.title: the role title as written.
- requirements.required_certifications / required_software: only ids from the enum lists in the schema, and only for things the JD requires (not "nice to have"). Anything required but not in the list goes in other_required_certifications / other_required_software in the JD's wording.
- requirements.required_qualifications: one entry for each qualification in required_certifications or other_required_certifications. name_as_written as the JD names it; canonical the enum id, using the specific id for shared letters ("CPA" in a US role is cpa_us) only when the JD or its location settles which one is meant. accept_equivalents: true for "CPA or equivalent", "or international equivalent", "ACCA / ACA / CA"; false when the JD rules equivalents out, e.g. "active US CPA licence required" or a licence needed to sign US audits. equivalents_stated is true when the JD says either of those, false when it just names the qualification (accept_equivalents is then true). quote is the JD's own words, verbatim. A choice between qualifications of different kinds ("CPA or CMA") is not a single requirement: leave it in must_haves only.
- requirements.min_years_experience: the fewest total years of professional experience the JD accepts, as a whole number: 5 for "5+ years" or "minimum 5 years", 1 for "1-4 years". When one requirement gives several figures ("10 years, at least 5 in manufacturing") use the overall one. null when the JD gives no number ("a couple of years", "experienced") or only asks for it under a preferred heading.
- requirements.preferred_certifications / preferred_software: enum ids for certifications and products the JD names only as preferred, a bonus or "nice to have"; anything not in the list goes in other_preferred_certifications / other_preferred_software in the JD's wording. Alternatives are all listed ("CPP or FPC" is both). Never repeat something that is required.
- requirements.industries: ids from the enum for the industry context of the role; other_industries for anything else.
- requirements.must_haves: every hard requirement, each a verbatim fragment of the JD. nice_to_haves: preferred-but-optional items, also verbatim. An item is in one list or the other, never both.
- requirements.timezone: the IANA zone the role operates in, from the location or stated hours; null if unstated.
- requirements.starts_on: an ISO date only when the JD states a start date.

Return only the JSON object."""

RERANK_SYSTEM = f"""You score candidates for a finance and accounting role against a fixed rubric.

You are given the role and a list of candidates, each with an id and the text to judge. For every candidate, give each dimension of the rubric a level from 0 to 4, with one sentence of evidence from the candidate's text for the level you chose. Rules:
- Judge only what the text shows. A claim with no supporting detail is listed, not evidenced, and something the text does not mention is not met.
- Pick the level whose description fits best. There are no half levels, and one dimension does not make up for another: score each on its own question.
- Whether a dimension applies is a fact about the role, not the candidate. Where the rubric says a dimension is null, it is null for every candidate (the evidence says why); otherwise it is a level for every candidate.
- Do not give an overall score. It is computed from your levels.

The rubric:

{rubric.prompt_text()}

Qualifications from other countries: when the role accepts equivalents, a fully qualified accountant from another jurisdiction (ACA, ACCA, CA, a Canadian or Australian CPA) meets a CPA requirement, and the reverse. Count it as met in must_have_coverage; do not mark such a candidate down for the letters. An equivalent qualification is not equivalent experience, though: exposure to the accounting framework the role works under (US GAAP versus IFRS or UK GAAP) is scored in experience_depth, from what the candidate has actually done, and is named in a reason when it is the gap. Part-qualified, a management accounting qualification (CIMA, CMA) or technician level (AAT) does not meet a requirement for a fully qualified accountant.

Also give each candidate one to three short reasons in plain language a recruiter could read: the strongest point in their favour and the main gap.

Return one entry for every candidate id given, each id exactly once. Return only the JSON object."""


class ResumeExtraction(BaseModel):
    model_config = ConfigDict(extra="forbid")

    contact: Contact
    profile: CandidateProfile


class JDExtraction(BaseModel):
    model_config = ConfigDict(extra="forbid")

    company: str | None = None
    requirements: RoleRequirements


MAX_CANDIDATE_CHARS = 20_000


class RerankCandidate(BaseModel):
    model_config = ConfigDict(extra="forbid")

    id: str = Field(min_length=1, max_length=200, description="Opaque id echoed back in the result.")
    text: str = Field(
        min_length=1,
        max_length=MAX_CANDIDATE_CHARS,
        description="Resume text or a rendered profile; whatever the model should judge.",
    )


def _dimension(key: str) -> Any:
    return Field(description=rubric.BY_KEY[key].question)


class DimensionScore(BaseModel):
    """A candidate's level on a dimension the rubric always scores."""

    model_config = ConfigDict(extra="forbid")

    evidence: str = Field(description="One sentence: what in the candidate's text decides the level.")
    level: rubric.Level = Field(description="The rubric level, 0 (none) to 4 (full).")


class OptionalDimensionScore(BaseModel):
    """A candidate's level on a dimension the role may give nothing to score against."""

    model_config = ConfigDict(extra="forbid")

    evidence: str = Field(
        description="One sentence: what in the candidate's text decides the level, or why the dimension is null."
    )
    level: rubric.Level | None = Field(
        description="The rubric level, 0 (none) to 4 (full); null when the dimension does not apply to the role."
    )


class DimensionScores(BaseModel):
    """One level per dimension of the rubric (app/rubric.py, docs/rerank-rubric.md)."""

    model_config = ConfigDict(extra="forbid")

    must_have_coverage: OptionalDimensionScore = _dimension("must_have_coverage")
    experience_depth: DimensionScore = _dimension("experience_depth")
    software_fluency: OptionalDimensionScore = _dimension("software_fluency")
    industry_fit: OptionalDimensionScore = _dimension("industry_fit")
    nice_to_haves: OptionalDimensionScore = _dimension("nice_to_haves")

    def levels(self) -> dict[str, int | None]:
        return {d.key: getattr(self, d.key).level for d in rubric.DIMENSIONS}


class RerankJudgement(BaseModel):
    """What the model returns for a candidate: levels and reasons, no overall score."""

    model_config = ConfigDict(extra="forbid")

    id: str
    dimensions: DimensionScores
    reasons: list[str] = Field(default_factory=list)


class RerankOutput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    results: list[RerankJudgement]


class RerankResult(BaseModel):
    model_config = ConfigDict(extra="forbid")

    id: str
    score: float = Field(
        ge=0.0, le=1.0, description="Computed from `dimensions` by the rubric's formula; never the model's own number."
    )
    dimensions: DimensionScores
    reasons: list[str] = Field(default_factory=list)


def parse_resume(provider: Provider, text: str, today: date | None = None) -> ResumeExtraction:
    user = f"Today is {(today or date.today()).isoformat()}.\n\nResume:\n\n{text}"
    return ground_resume(llm.complete_json(provider, RESUME_SYSTEM, user, ResumeExtraction), text)


# Without a date or a stated length of experience there is nothing to count years from.
_TENURE = re.compile(r"(?<!\d)(?:19|20)\d\d(?!\d)|\b(?:years?|yrs?)\b", re.I)


_MIN_PHONE_DIGITS = 7
_NATIONAL_DIGITS = 10


# How a resume says a role has not ended.
_PRESENT = re.compile(r"\b(?:present|current(?:ly)?|now|ongoing|to date)\b", re.I)


def ground_resume(out: ResumeExtraction, text: str) -> ResumeExtraction:
    """Remove everything the resume text does not carry.

    The prompt tells the model not to invent; this makes it hold. Values that
    are read off the page (contact details, certifications, software, named
    standards, job titles, employers, years) must be findable in the text and
    are dropped, or set to null, when they are not. Values the model derives
    (headline, skills, industries, availability, time zone) are left alone.
    Each removal is logged."""
    tax = taxonomy.load()
    dropped: list[str] = []

    def keep(field: str, value: str, grounded: bool) -> bool:
        if not grounded:
            dropped.append(f"{field}={value!r}")
        return grounded

    def named(field: str, value: str | None) -> str | None:
        return value if value is not None and keep(field, value, in_text(value, text)) else None

    contact = out.contact
    phone = contact.phone
    if phone is not None:
        # A number the model wrote with a country code the resume leaves out is
        # still the resume's number: the last ten digits decide.
        digits, page = re.sub(r"\D", "", phone), re.sub(r"\D", "", text)
        grounded = len(digits) >= _MIN_PHONE_DIGITS and (
            digits in page or (len(digits) > _NATIONAL_DIGITS and digits[-_NATIONAL_DIGITS:] in page)
        )
        phone = phone if keep("contact.phone", phone, grounded) else None
    contact = Contact(
        full_name=named("contact.full_name", contact.full_name),
        email=named("contact.email", contact.email),
        phone=phone,
        location=contact.location,
    )

    profile = out.profile
    positions: list[Position] = []
    for p in profile.positions:
        if not keep("positions.title", p.title, in_text(p.title, text)):
            continue
        start, end = (
            y if y is None or keep(f"positions.{name}", str(y), year_in_text(y, text)) else None
            for name, y in (("start_year", p.start_year), ("end_year", p.end_year))
        )
        current = p.current and keep("positions.current", p.title, _PRESENT.search(text) is not None)
        positions.append(
            p.model_copy(
                update={
                    "employer": named("positions.employer", p.employer),
                    "start_year": start,
                    "end_year": end,
                    "current": current,
                }
            )
        )
    # A qualification has to be on the page under the name given. Its quote
    # and year are replaced or cleared rather than costing the whole record.
    quals: list[Qualification] = []
    for q in profile.qualifications:
        if not keep("qualifications", q.name_as_written, in_text(q.name_as_written, text)):
            continue
        quote = q.quote if q.quote is not None and in_text(q.quote, text) else line_with(q.name_as_written, text)
        year = q.year_obtained
        if year is not None and not keep("qualifications.year_obtained", str(year), year_in_text(year, text)):
            year = None
        body = q.issuing_body
        if (
            body is not None
            and q.canonical is not None
            and body == tax.term("certifications", q.canonical).issuing_body
        ):
            pass  # the taxonomy's name for the body, not a claim about the text
        elif body is not None and not keep("qualifications.issuing_body", body, in_text(body, text)):
            body = None
        quals.append(q.model_copy(update={"quote": quote, "year_obtained": year, "issuing_body": body}))
    held = {q.canonical for q in quals if q.status == "qualified"}
    years = profile.years_experience
    if years is not None and not keep("years_experience", str(years), _TENURE.search(text) is not None):
        years = None
    profile = CandidateProfile.model_validate(
        profile.model_dump()
        | {
            "years_experience": years,
            "positions": positions,
            "qualifications": quals,
            # An id that came from a qualification record stands or falls with the record.
            "certifications": [
                c
                for c in profile.certifications
                if keep("certifications", c, mentions(text, tax.term("certifications", c)))
                and (c in held or not any(q.canonical == c for q in profile.qualifications))
            ],
            "software": [s for s in profile.software if keep("software", s, mentions(text, tax.term("software", s)))],
            "other_certifications": [
                c for c in profile.other_certifications if keep("other_certifications", c, in_text(c, text))
            ],
            "other_software": [s for s in profile.other_software if keep("other_software", s, in_text(s, text))],
            "gaap_exposure": [g for g in profile.gaap_exposure if keep("gaap_exposure", g, in_text(g, text))],
        }
    )
    if dropped:
        log.warning("parse_resume: dropped values the resume does not carry: %s", ", ".join(dropped))
    return ResumeExtraction(contact=contact, profile=profile)


def parse_jd(provider: Provider, text: str) -> JDExtraction:
    user = f"Job description:\n\n{text}"
    return ground_jd(llm.complete_json(provider, JD_SYSTEM, user, JDExtraction), text)


# How a JD may write a minimum without digits: "five years", "a minimum of ten years".
_NUMBER_WORDS = (
    "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen "
    "sixteen seventeen eighteen nineteen twenty"
)


def _years_in_text(years: int, text: str) -> bool:
    if re.search(rf"(?<![\d.,]){years}(?![\d,]|\.\d)", text):
        return True
    words = _NUMBER_WORDS.split()
    return 0 < years <= len(words) and in_text(words[years - 1], text)


def ground_jd(out: JDExtraction, text: str) -> JDExtraction:
    """Remove the structured requirements the JD text does not carry.

    The must-haves are hard filters, so one the model invented would silently
    exclude candidates. A required or preferred certification or product must
    be named in the JD (by label or taxonomy alias), an `other_*` entry and a
    qualification's name must be there verbatim, and a minimum number of years
    must be a number the JD writes. What fails is dropped, or set to null,
    and logged; a qualification's invented quote is replaced by the line that
    names it. The values the model derives or copies out as prose (title,
    industries, time zone, start date, must_haves, nice_to_haves) are left alone."""
    tax = taxonomy.load()
    dropped: list[str] = []

    def keep(field: str, value: str, grounded: bool) -> bool:
        if not grounded:
            dropped.append(f"{field}={value!r}")
        return grounded

    req = out.requirements

    def named(field: str, kind: taxonomy.Kind, ids: list[str]) -> list[str]:
        return [i for i in ids if keep(field, i, mentions(text, tax.term(kind, i)))]

    def verbatim(field: str, values: list[str]) -> list[str]:
        return [v for v in values if keep(field, v, in_text(v, text))]

    quals: list[RequiredQualification] = []
    for q in req.required_qualifications:
        if not keep("required_qualifications", q.name_as_written, in_text(q.name_as_written, text)):
            continue
        quote = q.quote if q.quote is not None and in_text(q.quote, text) else line_with(q.name_as_written, text)
        quals.append(q.model_copy(update={"quote": quote}))
    kept = {q.canonical for q in quals}
    years = req.min_years_experience
    if years is not None and not keep("min_years_experience", str(years), _years_in_text(years, text)):
        years = None
    requirements = RoleRequirements.model_validate(
        req.model_dump()
        | {
            "required_qualifications": quals,
            # An id that came from a qualification record stands or falls with the record.
            "required_certifications": [
                c
                for c in named("required_certifications", "certifications", req.required_certifications)
                if c in kept or not any(q.canonical == c for q in req.required_qualifications)
            ],
            "other_required_certifications": verbatim(
                "other_required_certifications", req.other_required_certifications
            ),
            "required_software": named("required_software", "software", req.required_software),
            "other_required_software": verbatim("other_required_software", req.other_required_software),
            "min_years_experience": years,
            "preferred_certifications": named(
                "preferred_certifications", "certifications", req.preferred_certifications
            ),
            "other_preferred_certifications": verbatim(
                "other_preferred_certifications", req.other_preferred_certifications
            ),
            "preferred_software": named("preferred_software", "software", req.preferred_software),
            "other_preferred_software": verbatim("other_preferred_software", req.other_preferred_software),
        }
    )
    if dropped:
        log.warning("parse_jd: dropped values the job description does not carry: %s", ", ".join(dropped))
    return JDExtraction(company=out.company, requirements=requirements)


def rerank(provider: Provider, role: str, candidates: list[RerankCandidate]) -> list[RerankResult]:
    """Score every candidate against the role; returned best first.

    The model gives the rubric levels; the score is `rubric.overall` of them,
    so it can be recomputed from the `dimensions` of any result."""
    expected = {c.id for c in candidates}

    def check(out: RerankOutput) -> str | None:
        got = [r.id for r in out.results]
        problems: list[str] = []
        if missing := sorted(expected - set(got)):
            problems.append(f"missing ids {missing}")
        if unknown := sorted(set(got) - expected):
            problems.append(f"unknown ids {unknown}")
        if dupes := sorted({i for i in got if got.count(i) > 1}):
            problems.append(f"duplicate ids {dupes}")
        if problems:
            return "results must contain every candidate id exactly once: " + "; ".join(problems)
        # Scores are only comparable when every candidate was scored on the same dimensions.
        levels = [r.dimensions.levels() for r in out.results]
        mixed = [d.key for d in rubric.DIMENSIONS if len({lv[d.key] is None for lv in levels}) > 1]
        if mixed:
            return (
                "whether a dimension applies depends on the role, not the candidate: "
                f"{', '.join(mixed)} must be null for every candidate or for none"
            )
        return None

    rendered = "\n\n".join(f"=== candidate id: {c.id} ===\n{c.text}" for c in candidates)
    user = f"Role:\n\n{role}\n\nCandidates ({len(candidates)}):\n\n{rendered}"
    out = llm.complete_json(provider, RERANK_SYSTEM, user, RerankOutput, check=check)

    def rank(r: RerankJudgement) -> tuple[float, float]:
        # Candidates held at the same must-have cap are ordered by what they scored before it.
        levels = r.dimensions.levels()
        return rubric.overall(levels), rubric.weighted(levels)

    return [
        RerankResult(id=r.id, score=rubric.overall(r.dimensions.levels()), dimensions=r.dimensions, reasons=r.reasons)
        for r in sorted(out.results, key=rank, reverse=True)
    ]
