"""What a professional qualification on a resume or in a JD actually is.

A US job description asks for a "CPA"; a UK candidate holds an ACA, an ACCA or
a CA instead. The parsers therefore record the qualification as written and
never turn one into another: an ACA is stored as `aca_icaew`, not as `cpa`.
Whether it satisfies a requirement is decided later, by the equivalence groups
in infra/taxonomy.json (app/matching.py).

This module is the deterministic half of that extraction, run by the schema
validators on whatever the model returned:

* `settle` fixes the taxonomy id. The name as written decides which family
  the qualification is in ("ACA" can only ever be an ACA), so a model that
  answers "cpa" for an ACA is overruled. Letters that several bodies share
  ("CPA", "CA", "ACA") stay on the ambiguous id unless the issuing body or a
  jurisdiction settles which one it is.
* `status_from` downgrades a "qualified" the text itself contradicts:
  "ACCA finalist" and "part-qualified ACCA" are not ACCA.
"""

from __future__ import annotations

import re
from typing import Literal

from app.taxonomy import Taxonomy, Term

Status = Literal["qualified", "part_qualified", "in_progress"]

# Words around a qualification's name that say how far along the holder is,
# not which qualification it is: "Part-qualified ACCA", "ACA finalist",
# "CPA candidate", "CIMA student".
_STATUS_WORDS = re.compile(
    r"\b(?:part[- ]qualified|fully[- ]qualified|newly[- ]qualified|qualified|finalist|candidate|student|trainee"
    r"|member|fellow|in progress|studying(?: for| towards?)?|pursuing|working towards?)\b",
    re.IGNORECASE,
)
_PART_QUALIFIED = re.compile(r"\bpart[- ]qualifi(?:ed|cation)\b|\bfinalist\b|\bpapers (?:passed|remaining)\b", re.I)
_IN_PROGRESS = re.compile(
    r"\b(?:studying|in progress|pursuing|working towards?|candidate|student|trainee|eligible|sitting"
    r"|exams? (?:passed|remaining|pending|scheduled|sections?)|passed \w+ of|expected)\b",
    re.I,
)
# The words that qualify one name stop at the next comma or "and": in "AAT
# qualified and part-qualified ACCA" only the ACCA is part-qualified.
_CLAUSE = re.compile(r"[;,|•\n]|\.\s|\band\b")
# A state or province code ends a location ("Austin, TX", "Toronto, ON | ..."); the
# "CA" in "CPA, CA, CIA" does not.
_REGION = r"(?<=, )(?:{})(?=[ \t]*(?:$|[|\u2022\u00b7]|\d{{5}}))"
_NAME = r"(?<![A-Za-z0-9]){}(?![A-Za-z0-9])"


def resolve_name(tax: Taxonomy, name: str) -> str | None:
    """The taxonomy id a qualification's written name belongs to, ignoring the
    words that only describe progress. May be an ambiguous id."""
    return tax.resolve("certifications", name) or tax.resolve("certifications", _STATUS_WORDS.sub(" ", name))


def _named_body(candidates: list[Term], evidence: str) -> Term | None:
    """The candidate whose issuing body the evidence names; the longest name wins."""
    best: tuple[int, Term] | None = None
    for term in candidates:
        for alias in term.body_aliases:
            if re.search(_NAME.format(re.escape(alias)), evidence, re.IGNORECASE) and (
                best is None or len(alias) > best[0]
            ):
                best = (len(alias), term)
    return best[1] if best else None


def body_in(tax: Taxonomy, term_id: str, text: str) -> str | None:
    """The issuing body the text names for this qualification or one of its
    variants, as the text writes it; None if it names none."""
    for term in (tax.term("certifications", term_id), *tax.variants(term_id)):
        for alias in sorted(term.body_aliases, key=len, reverse=True):
            if m := re.search(_NAME.format(re.escape(alias)), text, re.IGNORECASE):
                return m.group()
    return None


def guess_jurisdiction(tax: Taxonomy, text: str) -> str | None:
    """The first jurisdiction the text names: a country ("London, UK", "any US
    state") or a state or province code after a comma ("Austin, TX")."""
    found: list[tuple[int, str]] = []
    for j in tax.jurisdictions():
        names = (re.search(_NAME.format(re.escape(name)), text) for name in (j.id, *j.aliases))
        found.extend((m.start(), j.id) for m in names if m)
        if j.regions and (m := re.search(_REGION.format("|".join(j.regions)), text, re.MULTILINE)):
            found.append((m.start(), j.id))
    return min(found)[1] if found else None


def settle(
    tax: Taxonomy,
    name_as_written: str,
    canonical: str | None,
    issuing_body: str | None,
    jurisdiction: str | None,
    *,
    hint: str | None = None,
) -> tuple[str | None, str | None, str | None]:
    """(canonical, issuing_body, jurisdiction) for one qualification.

    `canonical` is the model's answer and is kept only when the written name
    supports it. `hint` is a jurisdiction id from the context (the candidate's
    time zone, the JD's location), used only when nothing in the
    qualification itself says where it is from.
    """
    body = (issuing_body or "").strip() or None
    where = tax.jurisdiction(jurisdiction) or (jurisdiction or "").strip() or None
    base = resolve_name(tax, name_as_written)
    if base is None:
        return None, body, where
    # Only shared letters leave anything to settle. A name that is already
    # specific ("US CPA", "CPA, CA", "ACCA") is that qualification, whatever
    # the model or the candidate's address suggests.
    variants = tax.variants(base)
    term = next((t for t in variants if t.id == canonical), tax.term("certifications", base))
    if variants:
        place = tax.jurisdiction(where) or hint
        local = [t for t in variants if place in t.jurisdictions]
        if len(local) > 1:
            # "CA" in the UK is the ICAS designation; ICAEW members write ACA.
            local = [t for t in local if t.variant_of[0] == base]
        # A body named next to the qualification beats everything else,
        # including a variant the model picked: "CPA (CPA Ontario)" is Canadian.
        if named := _named_body(variants, f"{name_as_written} {body or ''}"):
            term = named
        elif term.id == base and len(local) == 1:
            term = local[0]
    return term.id, term.issuing_body or body, term.jurisdiction or where


def status_from(name_as_written: str, quote: str | None, status: Status) -> Status:
    """The model's status, unless the words next to the qualification say it
    is not held yet. Only ever a downgrade."""
    if status != "qualified":
        return status
    clauses = [c for c in _CLAUSE.split(quote or "") if name_as_written.lower() in c.lower()]
    context = " ".join([name_as_written, *clauses]) if clauses or not quote else f"{name_as_written} {quote}"
    if _PART_QUALIFIED.search(context):
        return "part_qualified"
    if _IN_PROGRESS.search(context):
        return "in_progress"
    return status


def display(
    tax: Taxonomy, canonical: str | None, name_as_written: str, issuing_body: str | None, where: str | None
) -> str:
    """ "ACA (ICAEW, UK)": the name as the candidate wrote it, then who awarded it and where."""
    name = _STATUS_WORDS.sub(" ", name_as_written)
    name = " ".join(name.split()).strip(" ,-") or name_as_written
    if canonical is not None and tax.resolve("certifications", name) is None:
        name = tax.term("certifications", canonical).label
    detail = ", ".join(d for d in (issuing_body, where) if d and d.lower() != name.lower())
    return f"{name} ({detail})" if detail else name
