"""The prompts and output models behind /parse-resume, /parse-jd and /rerank.

Each function takes a Provider and returns a validated model; the HTTP layer
in app/main.py only wraps them in request and response types and maps
LLMError to a status code. Everything the model is asked for is described by
a Pydantic model whose schema is sent along with the prompt, so the shapes
here are also the contract with the model.
"""

from __future__ import annotations

from datetime import date

from pydantic import BaseModel, ConfigDict, Field

from app import llm
from app.llm import Provider
from app.schemas import CandidateProfile, Contact, RoleRequirements

RESUME_SYSTEM = """You extract structured data from resumes for a finance and accounting recruiting product.

Read the resume text and fill in the schema. Rules:
- contact: the candidate's name, email, phone and location exactly as written in the header. Use null for anything absent.
- profile.headline: one line that says what the candidate is, e.g. "Senior Accountant, CPA - NetSuite multi-entity close".
- profile.years_experience: whole years of professional experience from the first professional role to the date given as "today". Exclude internships and study. null if it cannot be worked out.
- profile.certifications / profile.software / profile.industries: only ids from the enum lists in the schema. Use a candidate's own wording for an item not in the list by putting it in other_certifications / other_software / other_industries instead. Never invent a certification or tool the resume does not name.
- profile.skills: 6 to 14 short skills the resume evidences, lower case except acronyms, e.g. "month-end close", "ASC 606 revenue recognition".
- profile.languages: ISO 639-1 codes, "en" if nothing else is stated.
- profile.availability: immediate | two_weeks | one_month | unavailable | unknown, from what the resume says. available_from: an ISO date only when the resume states one.
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
    return llm.complete_json(provider, RESUME_SYSTEM, user, ResumeExtraction)


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
