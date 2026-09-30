"""The text that is embedded for a candidate profile and for a role.

A role's vector is compared with profile vectors by cosine distance, which
only means something when both were computed from text of the same kind. A
raw JD next to a JSON dump of a profile is not: the distance is then mostly
"prose versus JSON". So both sides are rendered here into one layout, the
same labelled lines in the same order, each filled from the field of its own
model that answers the same question:

    line            profile (CandidateProfile)          role (RoleRequirements)
    Role            headline, then the titles held      title
    Qualifications  certifications held                 required, then preferred
    Software        software                            required, then preferred
    Industries      industries                          industries
    Standards       gaap_exposure                       (none; a JD names them in its must-haves)
    Skills          skills                              must_haves, then nice_to_haves,
                                                        without the logistics (see below)

Taxonomy ids are written as their labels ("NetSuite", not `netsuite`), so
both sides use the same words whatever the resume or the JD called the thing.
A line with nothing to say is left out. Only what describes the work is in
the text. What a hard filter decides by comparison is not: time zone, start
date, availability, and years of experience, where the role's number is a
minimum and the profile's a total, so "3 years" next to "11 years" would read
as a difference when it is a pass. Nor is what says nothing about fit:
employer names, contact details, languages.

A JD's must-haves are free text and mix the two: "Central time; on-site in
Chicago" sits next to "Standard costing under US GAAP". The clauses (an entry
is split at ";") that are about where, when and what hours are recognised by
their wording (LOGISTICS) and dropped. That is a word list, not a parse: a
clause it misses stays in as a little noise, and the full must-haves still
reach the rerank untouched.

The rendering is a pure function of the model, so an unchanged profile or
role always embeds to the same vector (and hits the embedding cache).
"""

from __future__ import annotations

import re
from collections.abc import Iterable

from app import taxonomy
from app.schemas import CandidateProfile, RoleRequirements
from app.taxonomy import Kind

# A must-have clause about location, working hours, start date or the right
# to work: things a candidate's skills cannot answer.
LOGISTICS = re.compile(
    r"""
      \b(pacific|mountain|central|eastern)\s+time\b | \btime\s*zone\b | \bbusiness\s+hours\b
    | \bon-?site\b | \bhybrid\b | \bremote\b | \bcommutable\b | \brelocat
    | \b(based|located)\s+in\b
    | \bright\s+to\s+work\b | \bwork\s+authori[sz]ation\b | \bvisa\b
    | \b(able|available|availability)\s+to\s+start\b | \bstart\s+(within|immediately|on\s+or\s+before|date)\b
    """,
    re.IGNORECASE | re.VERBOSE,
)


def profile_text(profile: CandidateProfile) -> str:
    """The document embedded for a candidate profile."""
    return _render(
        [
            ("Role", _unique([profile.headline, *(p.title for p in profile.positions)])),
            ("Qualifications", _labels("certifications", profile.certifications, profile.other_certifications)),
            ("Software", _labels("software", profile.software, profile.other_software)),
            ("Industries", _labels("industries", profile.industries, profile.other_industries)),
            ("Standards", _unique(profile.gaap_exposure)),
            ("Skills", _unique(profile.skills)),
        ]
    )


def role_text(requirements: RoleRequirements) -> str:
    """The document embedded for a role, from the JD parser's output."""
    r = requirements
    return _render(
        [
            ("Role", _unique([r.title])),
            (
                "Qualifications",
                _labels(
                    "certifications",
                    [*r.required_certifications, *r.preferred_certifications],
                    [*r.other_required_certifications, *r.other_preferred_certifications],
                ),
            ),
            (
                "Software",
                _labels(
                    "software",
                    [*r.required_software, *r.preferred_software],
                    [*r.other_required_software, *r.other_preferred_software],
                ),
            ),
            ("Industries", _labels("industries", r.industries, r.other_industries)),
            ("Skills", _unique(_about_the_work([*r.must_haves, *r.nice_to_haves]))),
        ]
    )


def _render(lines: list[tuple[str, list[str]]]) -> str:
    return "\n".join(f"{label}: {'; '.join(values)}" for label, values in lines if values)


def _about_the_work(entries: Iterable[str]) -> list[str]:
    """Each entry without its logistics clauses; an entry that was nothing else is gone."""
    kept: list[str] = []
    for entry in entries:
        clauses = [c.strip() for c in entry.split(";") if c.strip() and not LOGISTICS.search(c)]
        if clauses:
            kept.append("; ".join(clauses))
    return kept


def _labels(kind: Kind, ids: Iterable[str], other: Iterable[str]) -> list[str]:
    """Taxonomy labels for the ids, then the free-text entries the taxonomy does not know."""
    tax = taxonomy.load()
    return _unique([*(tax.term(kind, i).label for i in ids), *other])


def _unique(values: Iterable[str | None]) -> list[str]:
    """Non-blank values with whitespace collapsed, first occurrence of each (case-insensitive) kept."""
    seen: set[str] = set()
    out: list[str] = []
    for value in values:
        text = " ".join((value or "").split())
        if text and text.casefold() not in seen:
            seen.add(text.casefold())
            out.append(text)
    return out
