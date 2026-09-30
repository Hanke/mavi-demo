"""Ask Claude to write the resume text for each planned slot.

The plan decides the facts; the model writes prose that carries them: a
resume the parser can extract the same structured profile from, with
sentences specific enough to quote as evidence. Every certification and
software id in the spec must appear in the resume by its display name or a
known alias, and a batch that misses one is retried for those slots only.

Each request's answer is kept in the response cache (app/cache.py) under the
model, the brief and the specs, so re-running a generation that already ran
(or one that died half way) only pays for the batches it has not seen. The
specs carry today's date, so that holds within a day.
"""

from __future__ import annotations

import fcntl
import json
import re
import sys
from collections.abc import Iterable
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from datetime import date
from pathlib import Path
from typing import Any, cast

import anthropic
from pydantic import BaseModel, ConfigDict, Field, ValidationError

from app import cache as cachemod
from app import taxonomy
from app.cache import Cache
from app.schemas import CandidateProfile
from app.seedgen.plan import PLAN_SEED, Slot

MODEL = "claude-opus-5-5"
BATCH_SIZE = 5
MAX_ATTEMPTS = 3
WORKERS = 4
MIN_WORDS = 200

SYSTEM_PROMPT = """You write synthetic resumes for a demo of a finance and accounting recruiting product.

Everything you write is fictional. Never use the name of a real company, a real person, a real school or a real street address. Invent plausible employer names ("Larkspur Dental Group", "Meridian Freight Co.", "Halcyon Robotics") and do not reuse an employer across the candidates in one batch. Schools can be generic ("State University", "a regional business school").

For each candidate spec, produce:

resume_text - plain text, 260 to 450 words, sections in this order:
  1. Header: full name on the first line, then "location | email | phone" (omit the phone if the spec has none).
  2. SUMMARY: two or three sentences. State the availability in the candidate's own words (e.g. "available immediately", "two weeks' notice", "available from mid-November", "not currently looking but open to the right role", or nothing specific for 'unknown').
  3. EXPERIENCE: two to four positions, most recent first. Each has employer, title, city, and a month/year range ("Mar 2021 - Present"), then three to five bullets starting with "- ". The ranges must be contiguous, must not overlap, and must add up to the stated years of experience ending in the present (today's date is given in the spec). Someone who is 'unavailable' is still employed; do not end their current job.
  4. EDUCATION: one or two lines.
  5. CERTIFICATIONS: one line per certification, or "None".
  6. SOFTWARE: one line listing the tools.

Rules for the content:
- Mention every certification and every software product in the spec somewhere in the bullets or the summary, by the display name given or a common alias ("QBO", "NetSuite OneWorld", "S/4HANA", "Dynamics 365 Business Central", "Intacct" are all fine). The SOFTWARE and CERTIFICATIONS sections list them too.
- Cover every GAAP / domain topic in the spec in concrete terms: what the candidate did with it, at what scale. Bullets should read like evidence a recruiter could quote: entity counts, close timelines, ledger or revenue size, headcount, error rates.
- Use the industries in the spec for the employers. Match seniority to years: a two-year staff accountant does not lead a SOX programme; a controller does not describe data entry.
- Do not claim any certification or software that is not in the spec, unless you also list it in other_certifications / other_software. Tools outside our taxonomy are welcome there, at most three, e.g. "CCH Axcess", "Zuora", "Blackbaud Financial Edge", "Lacerte", "AuditBoard", "TeamMate", "Coupa", "Chargebee", "CTP".
- Write in the candidate's voice with regional spelling for candidates outside the US where it fits. Vary the writing style between candidates; not every resume should sound the same.

headline - one line under 90 characters, e.g. "Senior Accountant, CPA - NetSuite multi-entity close".
skills - 8 to 14 short free-text skills that appear in the resume, lower case except acronyms ("month-end close", "ASC 606 revenue recognition", "bank reconciliations"). Include each GAAP / domain topic from the spec.

Return exactly one entry per spec, with its slot number."""


class GeneratedCandidate(BaseModel):
    model_config = ConfigDict(extra="forbid")

    slot: int
    headline: str = Field(max_length=120)
    resume_text: str
    skills: list[str]
    other_software: list[str] = Field(default_factory=list)
    other_certifications: list[str] = Field(default_factory=list)


class GeneratedBatch(BaseModel):
    model_config = ConfigDict(extra="forbid")

    candidates: list[GeneratedCandidate]


# The JSON schema handed to the API. Kept explicit rather than derived from
# the pydantic model so it is strict (additionalProperties: false, every key
# required), which structured outputs need.
OUTPUT_SCHEMA: dict[str, Any] = {
    "type": "object",
    "properties": {
        "candidates": {
            "type": "array",
            "items": {
                "type": "object",
                "properties": {
                    "slot": {"type": "integer"},
                    "headline": {"type": "string"},
                    "resume_text": {"type": "string"},
                    "skills": {"type": "array", "items": {"type": "string"}},
                    "other_software": {"type": "array", "items": {"type": "string"}},
                    "other_certifications": {"type": "array", "items": {"type": "string"}},
                },
                "required": ["slot", "headline", "resume_text", "skills", "other_software", "other_certifications"],
                "additionalProperties": False,
            },
        }
    },
    "required": ["candidates"],
    "additionalProperties": False,
}


@dataclass
class Generated:
    """One finished candidate: the plan's facts plus the model's prose, validated."""

    slot: Slot
    resume_text: str
    profile: CandidateProfile

    def to_dict(self) -> dict[str, Any]:
        profile = self.profile.model_dump(mode="json")
        return {
            "id": self.slot.id,
            "full_name": self.slot.full_name,
            "email": self.slot.email,
            "phone": self.slot.phone or None,
            "location": self.slot.location,
            "source": self.slot.source,
            "status": self.slot.status,
            "available_in_days": self.slot.available_in_days,
            "resume_text": self.resume_text,
            "profile": profile,
        }


def _names_for(tax: taxonomy.Taxonomy, kind: taxonomy.Kind, ids: Iterable[str]) -> dict[str, str]:
    """id -> display name (label plus a couple of aliases) for the prompt."""
    out: dict[str, str] = {}
    for cid in ids:
        term = tax.term(kind, cid)
        aliases = [a for a in term.aliases[:3] if a.lower() != term.label.lower()]
        out[cid] = term.label + (f" (also: {', '.join(aliases)})" if aliases else "")
    return out


def _spec(slot: Slot, tax: taxonomy.Taxonomy, today: date, feedback: str | None) -> dict[str, Any]:
    spec: dict[str, Any] = {
        "slot": slot.index,
        "today": today.isoformat(),
        "full_name": slot.full_name,
        "email": slot.email,
        "phone": slot.phone or None,
        "location": slot.location,
        "current_or_target_title": slot.title,
        "years_experience": slot.years_experience,
        "certifications": _names_for(tax, "certifications", slot.certifications),
        "software": _names_for(tax, "software", slot.software),
        "gaap_and_domain_topics": slot.gaap,
        "industries": [tax.term("industries", i).label for i in slot.industries],
        "availability": slot.availability,
        "available_in_days": slot.available_in_days,
        "languages": slot.languages,
    }
    if feedback:
        spec["fix_from_previous_draft"] = feedback
    return spec


_WORD = r"(?<![A-Za-z0-9]){}(?![A-Za-z0-9])"


def mentions(text: str, term: taxonomy.Term) -> bool:
    """True when the resume names the term by label or alias. Short aliases
    ("CA", "EA", "SQL") are matched case-sensitively so "California" does not
    count as a Chartered Accountant."""
    for name in (term.label, *term.aliases):
        pattern = _WORD.format(re.escape(name))
        flags = 0 if len(name) <= 4 else re.IGNORECASE
        if re.search(pattern, text, flags):
            return True
    return False


def missing_mentions(slot: Slot, resume_text: str, tax: taxonomy.Taxonomy) -> list[str]:
    certs = [tax.term("certifications", cid) for cid in slot.certifications]
    software = [tax.term("software", sid) for sid in slot.software]
    missing = [f"certification {t.label}" for t in certs if not mentions(resume_text, t)]
    missing += [f"software {t.label}" for t in software if not mentions(resume_text, t)]
    return missing


def assemble(slot: Slot, gen: GeneratedCandidate) -> CandidateProfile:
    """The stored profile: plan facts, model prose. Validation runs the same
    taxonomy resolution the parser does, so the JSON on disk is exactly a
    CandidateProfile."""
    return CandidateProfile.model_validate(
        {
            "headline": gen.headline.strip(),
            "years_experience": slot.years_experience,
            "certifications": list(slot.certifications),
            "other_certifications": gen.other_certifications,
            "software": list(slot.software),
            "other_software": gen.other_software,
            "industries": list(slot.industries),
            "other_industries": [],
            "skills": [s.strip() for s in gen.skills if s.strip()],
            "languages": list(slot.languages),
            "availability": slot.availability,
            "available_from": None,
            "timezone": slot.timezone,
        }
    )


def check(slot: Slot, gen: GeneratedCandidate | None, tax: taxonomy.Taxonomy) -> tuple[Generated | None, list[str]]:
    """Validate one model-written entry against its slot. Returns the finished
    candidate, or the list of problems to send back."""
    if gen is None:
        return None, ["no entry returned for this slot"]
    problems = missing_mentions(slot, gen.resume_text, tax)
    words = len(gen.resume_text.split())
    if words < MIN_WORDS:
        problems.append(f"resume is only {words} words; write 260-450")
    if slot.full_name not in gen.resume_text:
        problems.append("resume must start with the candidate's full name")
    try:
        profile = assemble(slot, gen)
    except ValidationError as e:
        return None, [*problems, f"profile failed validation: {e}"]
    if problems:
        return None, problems
    return Generated(slot=slot, resume_text=gen.resume_text.strip() + "\n", profile=profile), []


def specs_for(slots: list[Slot], today: date | None = None) -> list[dict[str, Any]]:
    """The per-slot specs exactly as the API path sends them, for writing
    resumes outside the API (see `python -m app.seedgen specs`)."""
    tax = taxonomy.load()
    return [_spec(s, tax, today or date.today(), None) for s in slots]


def ingest(batch: GeneratedBatch, slots: list[Slot]) -> tuple[list[Generated], dict[int, list[str]]]:
    """Validate model-written entries produced outside the API path."""
    tax = taxonomy.load()
    by_index = {s.index: s for s in slots}
    good: list[Generated] = []
    bad: dict[int, list[str]] = {}
    for gen in batch.candidates:
        slot = by_index.get(gen.slot)
        if slot is None:
            bad[gen.slot] = ["not a slot in the plan"]
            continue
        done, problems = check(slot, gen, tax)
        if done is None:
            bad[gen.slot] = problems
        else:
            good.append(done)
    return good, bad


def merge_into_file(path: Path, generated: list[Generated], model: str) -> int:
    """Add or replace candidates in the committed JSON, keyed by id, under a
    file lock so parallel ingests do not lose each other's work. Returns the
    total count."""
    path.parent.mkdir(parents=True, exist_ok=True)
    lock = path.with_suffix(".lock")
    with open(lock, "w") as lf:
        fcntl.flock(lf, fcntl.LOCK_EX)
        existing: dict[str, dict[str, Any]] = {}
        if path.exists():
            data = cast(dict[str, Any], json.loads(path.read_text(encoding="utf-8")))
            for c in cast(list[dict[str, Any]], data.get("candidates", [])):
                existing[cast(str, c["id"])] = c
        for g in generated:
            existing[g.slot.id] = g.to_dict()
        candidates = [existing[k] for k in sorted(existing)]
        out: dict[str, Any] = {
            "$comment": (
                "Synthetic candidates for the demo seed, written by the model from the plan in "
                "ai/app/seedgen/plan.py. Every person, employer and contact detail is fictional. "
                "Regenerate with `make seed-generate`; render to SQL with `make seed-render`."
            ),
            "generator": {
                "model": model,
                "plan_seed": PLAN_SEED,
                "count": len(candidates),
                "generated_on": date.today().isoformat(),
            },
            "candidates": candidates,
        }
        path.write_text(json.dumps(out, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
        fcntl.flock(lf, fcntl.LOCK_UN)
    return len(candidates)


class Generator:
    def __init__(
        self,
        client: anthropic.Anthropic | None = None,
        today: date | None = None,
        model: str = MODEL,
        cache: Cache | None = None,
    ):
        # Zero-arg client: ANTHROPIC_API_KEY from the environment (the Makefile
        # sources .env), or an `ant auth login` profile.
        self.client = client or anthropic.Anthropic(timeout=600.0, max_retries=3)
        self.today = today or date.today()
        self.model = model
        if cache is None:
            from app.settings import get_settings

            cache = cachemod.from_settings(get_settings())
        self.cache = cache
        self.tax = taxonomy.load()
        self.usage_in = 0
        self.usage_out = 0

    def _call(self, slots: list[Slot], feedback: dict[int, str]) -> dict[int, GeneratedCandidate]:
        specs = [_spec(s, self.tax, self.today, feedback.get(s.index)) for s in slots]
        user = "Write resumes for these candidates:\n\n" + json.dumps(specs, indent=2, ensure_ascii=False)
        key = cachemod.key(
            "llm", provider="anthropic", model=self.model, system=SYSTEM_PROMPT, user=user, schema=OUTPUT_SCHEMA
        )
        text = self.cache.get_or_call(
            key, lambda: self._request(user, slots), kind="llm", provider="anthropic", model=self.model
        )
        batch = GeneratedBatch.model_validate_json(text)
        return {c.slot: c for c in batch.candidates}

    def _request(self, user: str, slots: list[Slot]) -> str:
        """One uncached API call; returns the response text."""
        response = self.client.beta.messages.create(
            model=self.model,
            max_tokens=16000,
            system=SYSTEM_PROMPT,
            messages=[{"role": "user", "content": user}],
            output_config={"effort": "medium", "format": {"type": "json_schema", "schema": OUTPUT_SCHEMA}},
            # A safety-classifier decline is re-run on a fallback model inside
            # the same call instead of failing the batch.
            betas=["server-side-fallback-2026-07-01"],
            fallbacks="default",
        )
        self.usage_in += response.usage.input_tokens
        self.usage_out += response.usage.output_tokens
        if response.stop_reason == "refusal":
            raise RuntimeError(f"batch {[s.index for s in slots]}: request refused ({response.stop_details})")
        if response.stop_reason == "max_tokens":
            raise RuntimeError(f"batch {[s.index for s in slots]}: output truncated; lower BATCH_SIZE")
        return next(b.text for b in response.content if b.type == "text")

    def batch(self, slots: list[Slot]) -> list[Generated]:
        """Generate one batch, retrying the slots whose resume fails a check."""
        done: dict[int, Generated] = {}
        pending = list(slots)
        feedback: dict[int, str] = {}
        for attempt in range(1, MAX_ATTEMPTS + 1):
            got = self._call(pending, feedback)
            still: list[Slot] = []
            for slot in pending:
                finished, problems = check(slot, got.get(slot.index), self.tax)
                if finished is not None:
                    done[slot.index] = finished
                    continue
                feedback[slot.index] = "The previous draft had these problems, fix them: " + "; ".join(problems)
                print(f"  slot {slot.index} attempt {attempt}: {'; '.join(problems)}", file=sys.stderr)
                still.append(slot)
            pending = still
            if not pending:
                break
        if pending:
            raise RuntimeError(f"slots {[s.index for s in pending]} still failing after {MAX_ATTEMPTS} attempts")
        return [done[s.index] for s in slots]

    def run(self, slots: list[Slot]) -> list[Generated]:
        batches = [slots[i : i + BATCH_SIZE] for i in range(0, len(slots), BATCH_SIZE)]
        results: list[list[Generated]] = [[] for _ in batches]

        def work(i: int) -> None:
            results[i] = self.batch(batches[i])
            print(
                f"batch {i + 1}/{len(batches)} done (slots {batches[i][0].index}-{batches[i][-1].index})",
                file=sys.stderr,
            )

        with ThreadPoolExecutor(max_workers=WORKERS) as pool:
            list(pool.map(work, range(len(batches))))
        return [g for batch in results for g in batch]
