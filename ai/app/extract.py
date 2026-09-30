"""The prompts and output models behind /parse-resume, /parse-jd and /rerank.

Each function takes a Provider and returns a validated model (the resume
parser also strips anything the text does not carry, see `ground_resume`); the HTTP layer
in app/main.py only wraps them in request and response types and maps
LLMError to a status code. Everything the model is asked for is described by
a Pydantic model whose schema is sent along with the prompt, so the shapes
here are also the contract with the model.
"""

from __future__ import annotations

import logging
import re
from datetime import date

from pydantic import BaseModel, ConfigDict, Field

from app import llm, taxonomy
from app.grounding import in_text, mentions, year_in_text
from app.llm import Provider
from app.schemas import CandidateProfile, Contact, Position, RoleRequirements

log = logging.getLogger(__name__)

RESUME_SYSTEM = """You extract structured data from resumes for a finance and accounting recruiting product.

Read the resume text and fill in the schema. Extract only what the resume states. When it does not state something, return null (or an empty list); never guess, and never fill a gap with something plausible. Rules:
- contact: the candidate's name, email, phone and location exactly as written. Use null for anything absent.
- profile.headline: one line that says what the candidate is, e.g. "Senior Accountant, CPA - NetSuite multi-entity close".
- profile.years_experience: whole years of professional experience from the first professional role to the date given as "today". Exclude internships and study. null if it cannot be worked out.
- profile.positions: every job in the work history, in the order the resume lists them. title and employer as written (employer null if none is named); start_year and end_year as four-digit years, null when the resume gives none. A role that runs to the present has current true and end_year null. When one date range carries two titles ("Staff Accountant, then Accountant"), return one position with the most recent title. Clients served within a self-employed or consulting role are not separate positions. Internships are positions.
- profile.certifications / profile.software / profile.industries: only ids from the enum lists in the schema. Use a candidate's own wording for an item not in the list by putting it in other_certifications / other_software / other_industries instead. Never invent a certification or tool the resume does not name. A certification counts only when it is held: exam progress, "candidate" or "studying for" is not one.
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
- requirements.industries: ids from the enum for the industry context of the role; other_industries for anything else.
- requirements.must_haves: every hard requirement, each a verbatim fragment of the JD. nice_to_haves: preferred-but-optional items, also verbatim. An item is in one list or the other, never both.
- requirements.timezone: the IANA zone the role operates in, from the location or stated hours; null if unstated.
- requirements.starts_on: an ISO date only when the JD states a start date.

Return only the JSON object."""

RERANK_SYSTEM = """You rank candidates for a finance and accounting role.

You are given the role and a list of candidates, each with an id and a summary. Score every candidate from 0.0 (no fit) to 1.0 (ideal) for this specific role, judging on the hard requirements first (certifications, software, seniority, location or time zone) and then on relevance of experience. Give each candidate one to three short reasons in plain language a recruiter could read.

Return one entry for every candidate id given, each id exactly once, ordered from best to worst. Return only the JSON object."""


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


class RerankResult(BaseModel):
    model_config = ConfigDict(extra="forbid")

    id: str
    score: float = Field(ge=0.0, le=1.0)
    reasons: list[str] = Field(default_factory=list)


class RerankOutput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    results: list[RerankResult]


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
    years = profile.years_experience
    if years is not None and not keep("years_experience", str(years), _TENURE.search(text) is not None):
        years = None
    profile = profile.model_copy(
        update={
            "years_experience": years,
            "positions": positions,
            "certifications": [
                c
                for c in profile.certifications
                if keep("certifications", c, mentions(text, tax.term("certifications", c)))
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
    return llm.complete_json(provider, JD_SYSTEM, user, JDExtraction)


def rerank(provider: Provider, role: str, candidates: list[RerankCandidate]) -> list[RerankResult]:
    """Score every candidate against the role; returned best first."""
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
        if not problems:
            return None
        return "results must contain every candidate id exactly once: " + "; ".join(problems)

    rendered = "\n\n".join(f"=== candidate id: {c.id} ===\n{c.text}" for c in candidates)
    user = f"Role:\n\n{role}\n\nCandidates ({len(candidates)}):\n\n{rendered}"
    out = llm.complete_json(provider, RERANK_SYSTEM, user, RerankOutput, check=check)
    # The model is asked for best-first; sorting makes the contract hold even when it did not comply.
    return sorted(out.results, key=lambda r: r.score, reverse=True)
