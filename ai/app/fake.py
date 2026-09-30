"""The key-free LLM provider behind `LLM_PROVIDER=fake`.

A stand-in for the chat model so the service, the eval and a demo run with no
API key and no network: every answer is a pure function of the prompt, built
with regexes and taxonomy lookups instead of a model. It is wired through
the same Provider seam as the real ones, so everything downstream (schema
validation, taxonomy resolution, the cache, the HTTP layer) runs for real.

It is not a parser. It finds what can be found mechanically (an email, the
taxonomy terms a text names, the bullets under a "Requirements" heading) and
leaves the rest null, so its eval scores are a floor, not a target.

The answer is chosen by the title of the schema the caller sends, and the
resume, JD and candidate texts are cut out of the user prompts app/extract.py
builds; tests/test_fake.py runs every fixture through both so a change to
those prompts that breaks this file fails there.
"""

from __future__ import annotations

import json
import re
from datetime import date
from typing import Any

from app import taxonomy
from app.llm import ProviderError
from app.seedgen.generate import mentions
from app.taxonomy import Kind

# Bump when the answers change, so cached answers from the old rules are not replayed.
FAKE_MODEL = "fake-1"

_RETRY_MARKER = "\n\nYour previous answer did not match the required schema."
_EMAIL = re.compile(r"[\w.+-]+@[\w-]+(?:\.[\w-]+)+")
_PHONE = re.compile(r"\+?\(?\d[\d\s().-]{8,}\d")
_PHONE_DIGITS = range(10, 14)
_YEAR = re.compile(r"(?<!\d)(19[7-9]\d|20\d\d)(?!\d)")
_TODAY = re.compile(r"^Today is (\d{4}-\d{2}-\d{2})\.")
_DASHES = r"\u2013\u2014\-"
_NAME_END = re.compile(rf"\s{{2,}}|\s[{_DASHES}|]+\s|,|\|")
_BULLET = re.compile(r"^\s*(?:[-*•>]|\d+[.)])\s+")
_MUST_HEADING = re.compile(
    r"^(requirements?|qualifications?|must[- ]haves?|required|what you(?:'ll)? (?:need|bring))\b", re.I
)
_NICE_HEADING = re.compile(r"^(preferred|nice[- ]to[- ]haves?|bonus|plus(?:es)?|desirable|desired)\b", re.I)
_CANDIDATE = re.compile(r"^=== candidate id: (.+) ===$", re.M)
_WORD = re.compile(r"[a-z][a-z0-9&+/-]{3,}")
_AVAILABILITY = (
    (
        "immediate",
        re.compile(r"\b(available immediately|immediately available|immediate start|start immediately)\b", re.I),
    ),
    ("two_weeks", re.compile(r"\b(two|2) weeks?\b", re.I)),
    ("one_month", re.compile(r"\b(one|1|a) month\b|\b(four|4) weeks\b|\b30 days\b", re.I)),
)
MAX_YEARS = 70
# How much of a rerank score is the role's taxonomy terms the candidate names; the rest is word overlap.
TERM_WEIGHT = 0.7


class FakeProvider:
    """Deterministic answers for the three schemas app/extract.py asks for."""

    name = "fake"
    model = FAKE_MODEL

    def complete(self, system: str, user: str, schema: dict[str, Any]) -> str:
        user = user.split(_RETRY_MARKER, 1)[0]
        title = schema.get("title")
        if title == "ResumeExtraction":
            return json.dumps(_resume(user))
        if title == "JDExtraction":
            return json.dumps(_jd(user))
        if title == "RerankOutput":
            return json.dumps(_rerank(user))
        raise ProviderError(f"fake: no answer defined for schema {title!r}")


def _named(kind: Kind, text: str) -> list[str]:
    """Ids of the taxonomy terms the text names by label or alias, in taxonomy order."""
    return [t.id for t in taxonomy.load().terms(kind) if mentions(text, t)]


def _after(prompt: str, marker: str) -> str:
    return prompt.split(marker, 1)[1] if marker in prompt else prompt


def _first_line(text: str) -> str:
    return next((line.strip() for line in text.splitlines() if line.strip()), "")


def _resume(prompt: str) -> dict[str, Any]:
    text = _after(prompt, "Resume:\n\n")
    today = date.fromisoformat(m.group(1)) if (m := _TODAY.match(prompt)) else date.today()

    name = _NAME_END.split(_first_line(text), maxsplit=1)[0].strip()
    if name.isupper():
        name = name.title()
    email = _EMAIL.search(text)
    phone = next(
        (p.group().strip() for p in _PHONE.finditer(text) if sum(c.isdigit() for c in p.group()) in _PHONE_DIGITS),
        None,
    )
    # The earliest year on the page stands in for the first professional role.
    years = [int(y) for y in _YEAR.findall(text) if int(y) <= today.year]
    availability = next((value for value, pattern in _AVAILABILITY if pattern.search(text)), "unknown")
    return {
        "contact": {
            "full_name": name or "Unknown",
            "email": email.group() if email else "",
            "phone": phone,
            "location": None,
        },
        "profile": {
            "headline": None,
            "years_experience": min(today.year - min(years), MAX_YEARS) if years else None,
            "certifications": _named("certifications", text),
            "other_certifications": [],
            "software": _named("software", text),
            "other_software": [],
            "industries": _named("industries", text),
            "other_industries": [],
            "skills": [],
            "languages": ["en"],
            "availability": availability,
            "available_from": None,
            "timezone": None,
        },
    }


def _jd(prompt: str) -> dict[str, Any]:
    text = _after(prompt, "Job description:\n\n")
    # Bullets belong to the nearest heading above them; only the two kinds of
    # heading the schema has a list for are kept.
    section: str | None = None
    must: list[str] = []
    nice: list[str] = []
    for raw in text.splitlines():
        line = raw.strip()
        if not line:
            continue
        if not _BULLET.match(raw):
            section = "must" if _MUST_HEADING.match(line) else "nice" if _NICE_HEADING.match(line) else None
            continue
        item = _BULLET.sub("", raw).strip().rstrip(".")
        if section == "must":
            must.append(item)
        elif section == "nice":
            nice.append(item)
    # A term named only under "Preferred" is not a requirement.
    required_in = "\n".join(must) if must else text
    title = re.split(rf"\s+[(|{_DASHES}]", _first_line(text), maxsplit=1)[0].strip()
    return {
        "company": None,
        "requirements": {
            "title": title or None,
            "required_certifications": _named("certifications", required_in),
            "other_required_certifications": [],
            "required_software": _named("software", required_in),
            "other_required_software": [],
            "industries": _named("industries", text),
            "other_industries": [],
            "must_haves": must,
            "nice_to_haves": nice,
            "timezone": None,
            "starts_on": None,
        },
    }


def _rerank(prompt: str) -> dict[str, Any]:
    head, *rest = _CANDIDATE.split(prompt)
    role = _after(head, "Role:\n\n")
    candidates = list(zip(rest[0::2], rest[1::2], strict=True))
    tax = taxonomy.load()
    wanted = [t for kind in ("certifications", "software") for t in tax.terms(kind) if mentions(role, t)]
    role_words = set(_WORD.findall(role.lower()))

    results: list[dict[str, Any]] = []
    for cid, text in candidates:
        has = [t.label for t in wanted if mentions(text, t)]
        lacks = [t.label for t in wanted if not mentions(text, t)]
        overlap = len(role_words & set(_WORD.findall(text.lower()))) / len(role_words) if role_words else 0.0
        score = TERM_WEIGHT * len(has) / len(wanted) + (1 - TERM_WEIGHT) * overlap if wanted else overlap
        reasons = [f"names {', '.join(has)}"] if has else []
        if lacks:
            reasons.append(f"does not name {', '.join(lacks)}")
        if not reasons:
            reasons.append(f"shares {overlap:.0%} of the role's wording")
        results.append({"id": cid, "score": round(min(score, 1.0), 3), "reasons": reasons})
    results.sort(key=lambda r: (-r["score"], r["id"]))
    return {"results": results}
