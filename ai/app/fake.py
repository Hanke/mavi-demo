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
from dataclasses import replace
from datetime import date
from typing import Any

from app import qualifications, taxonomy
from app.grounding import mentions, standards_named
from app.llm import ProviderError
from app.taxonomy import Kind

# Bump when the answers change, so cached answers from the old rules are not replayed.
FAKE_MODEL = "fake-3"

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
    return [t.id for t in taxonomy.load().terms(kind) if _names(text, t)]


def _names(text: str, term: taxonomy.Term) -> bool:
    """`mentions` by the term's own names only: "CPA" names the ambiguous cpa, not each of its variants."""
    return mentions(text, replace(term, inherited=()))


_SHORT = 4  # as in app.grounding: short names are matched case-sensitively
_YEAR_AFTER = r"[^\n]{{0,25}}?(?<!\d)((?:19|20)\d\d)(?!\d)(?!\s*[-\u2013\u2014])"
_NO_EQUIVALENTS = re.compile(r"\b(?:active|current|valid)\b[^\n]*\blicen[sc]e\b|\bno equivalents?\b", re.I)
_EQUIVALENTS = re.compile(r"\bequivalent", re.I)


# "a 40-person CPA firm" names an employer, not a licence; "ACA reporting" is the Affordable Care Act.
_NOT_A_HOLDING = re.compile(r"\s+(?:firms?|practices?|reporting|compliance|filings?|forms?)\b", re.I)
_SOMEONE_ELSE = re.compile(r"(?:client's|clients'|outside|external|their)\s$", re.I)
_STATE_LIST = re.compile(r",\s*[A-Z]{2}\b")
_CITY_BEFORE = re.compile(r"(?:[a-z]\w*,|\bin)\s$")


def _is_place(text: str, start: int, end: int) -> bool:
    """Two capitals that are a state, not a qualification: "Oakland, CA", "in CA, WA, OR"."""
    if end - start != 2:  # noqa: PLR2004
        return False
    return bool(_CITY_BEFORE.search(text[max(0, start - 40) : start]) or _STATE_LIST.match(text, end))


def _qualifications(text: str, header: str = "") -> list[dict[str, Any]]:
    """One record per certification the text names, as the text writes it.

    A name inside a longer one ("CA" in "CPA, CA") is not a second
    qualification. Of several mentions, the first that does not read as
    "part-qualified" or "studying for" is the one quoted; which body and
    jurisdiction it is from is left to the schema's validators, given the
    body the line names and the country in the header."""
    tax = taxonomy.load()
    spans: list[tuple[int, int, str]] = []
    for term in tax.terms("certifications"):
        for name in term.names:
            flags = 0 if len(name) <= _SHORT else re.IGNORECASE
            pattern = rf"(?<![A-Za-z0-9]){re.escape(name)}(?![A-Za-z0-9])"
            spans += [(m.start(), m.end(), term.id) for m in re.finditer(pattern, text, flags)]
    spans = [
        s
        for s in spans
        if not _NOT_A_HOLDING.match(text, s[1])
        and not _SOMEONE_ELSE.search(text[max(0, s[0] - 12) : s[0]])
        and not _is_place(text, s[0], s[1])
    ]
    spans = [s for s in spans if not any(o[0] <= s[0] and s[1] <= o[1] and o[1] - o[0] > s[1] - s[0] for o in spans)]
    where = qualifications.guess_jurisdiction(tax, header)
    records: dict[str, dict[str, Any]] = {}
    for start, end, term_id in sorted(spans):
        name = text[start:end]
        line = text[text.rfind("\n", 0, start) + 1 : (text.find("\n", end) + 1 or len(text) + 1) - 1]
        quote = _BULLET.sub("", line).strip()
        held = qualifications.status_from(name, quote, "qualified") == "qualified"
        if term_id in records and (records[term_id]["_held"] or not held):
            continue
        year = re.search(re.escape(name) + _YEAR_AFTER.format(), quote)
        records[term_id] = {
            "name_as_written": name,
            "canonical": term_id,
            "issuing_body": qualifications.body_in(tax, term_id, quote),
            "jurisdiction": where if tax.variants(term_id) else None,
            "status": "qualified",
            "year_obtained": int(year.group(1)) if year else None,
            "quote": quote,
            "_held": held,
        }
    return [{k: v for k, v in r.items() if k != "_held"} for r in records.values()]


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
            "full_name": name or None,
            "email": email.group() if email else None,
            "phone": phone,
            "location": None,
        },
        "profile": {
            "headline": None,
            "years_experience": min(today.year - min(years), MAX_YEARS) if years else None,
            "positions": [],
            "certifications": [],
            "other_certifications": [],
            "qualifications": _qualifications(text, "\n".join(text.strip().splitlines()[:3])),
            "software": _named("software", text),
            "other_software": [],
            "industries": _named("industries", text),
            "other_industries": [],
            "gaap_exposure": standards_named(text),
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
            "required_certifications": [],
            "other_required_certifications": [],
            "required_qualifications": [
                {
                    "name_as_written": q["name_as_written"],
                    "canonical": q["canonical"],
                    "accept_equivalents": not _NO_EQUIVALENTS.search(q["quote"]),
                    "equivalents_stated": bool(_NO_EQUIVALENTS.search(q["quote"]) or _EQUIVALENTS.search(q["quote"])),
                    "quote": q["quote"],
                }
                for q in _required(_qualifications(required_in))
            ],
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


def _required(records: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """In "CPA or equivalent (ACA, ACCA, CA)" the first name is the requirement
    and the rest are examples of the equivalents, not three more requirements."""
    out: list[dict[str, Any]] = []
    for q in records:
        if _EQUIVALENTS.search(q["quote"]) and any(o["quote"] == q["quote"] for o in out):
            continue
        out.append(q)
    return out


def _rerank(prompt: str) -> dict[str, Any]:
    head, *rest = _CANDIDATE.split(prompt)
    role = _after(head, "Role:\n\n")
    candidates = list(zip(rest[0::2], rest[1::2], strict=True))
    tax = taxonomy.load()
    wanted = [t for kind in ("certifications", "software") for t in tax.terms(kind) if _names(role, t)]
    role_words = set(_WORD.findall(role.lower()))

    results: list[dict[str, Any]] = []
    for cid, text in candidates:
        has = [t.label for t in wanted if _names(text, t)]
        lacks = [t.label for t in wanted if not _names(text, t)]
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
