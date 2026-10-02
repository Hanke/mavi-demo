"""The hard filter on qualifications, software and years of experience, with
the reason in words.

A role's required qualification is met by a candidate who holds it, or, when
the role accepts equivalents, one in the same equivalence group
(infra/taxonomy.json, `Taxonomy.acceptable`). Only fully held qualifications
are in `CandidateProfile.certifications`, so "part-qualified ACCA" fails
without a special case. The shortlist query does the same thing in SQL with
`p.certifications && <acceptable ids>` per requirement (see the README).
A minimum number of years is a plain comparison with the profile's
`years_experience`; a profile that does not say fails it, as it would in SQL.

Passing here says the candidate has a qualification of the right level. It
says nothing about experience: an ACA who has only ever reported under
FRS 102 passes a "CPA or equivalent" filter, and it is the rerank that scores
their US GAAP exposure.
"""

from __future__ import annotations

from dataclasses import dataclass, field

from app import qualifications, taxonomy
from app.schemas import CandidateProfile, Qualification, RequiredQualification, RoleRequirements
from app.taxonomy import Taxonomy

_STATUS_WORDS = {"part_qualified": "Part-qualified", "in_progress": "Studying for"}


@dataclass(frozen=True)
class QualificationCheck:
    required: str
    """Taxonomy id the role asks for."""
    passed: bool
    held: str | None
    """Taxonomy id of the candidate's qualification the explanation is about, if any."""
    explanation: str


@dataclass(frozen=True)
class HardFilterResult:
    passed: bool
    qualifications: list[QualificationCheck] = field(default_factory=list[QualificationCheck])
    missing_software: list[str] = field(default_factory=list[str])
    experience_short: str | None = None
    """Why the candidate falls under the role's minimum years, if they do."""

    @property
    def explanations(self) -> list[str]:
        tax = taxonomy.load()
        lines = [c.explanation for c in self.qualifications]
        lines += [f"Does not list {tax.term('software', s).label}" for s in self.missing_software]
        if self.experience_short:
            lines.append(self.experience_short)
        return lines


def _requirements(req: RoleRequirements) -> list[RequiredQualification]:
    """One record per required certification id. An id with no record (a role
    written before the records existed, or edited through the API as a bare
    list) accepts equivalents, which is the default the ticket sets."""
    tax = taxonomy.load()
    by_id = {r.canonical: r for r in req.required_qualifications if r.canonical is not None}
    return [
        by_id.get(cid) or RequiredQualification(name_as_written=tax.term("certifications", cid).label, canonical=cid)
        for cid in req.required_certifications
    ]


def _held(tax: Taxonomy, profile: CandidateProfile, canonical: str) -> str:
    record = next((q for q in profile.qualifications if q.canonical == canonical), None)
    return _shown(tax, record) if record else tax.term("certifications", canonical).label


def _shown(tax: Taxonomy, q: Qualification) -> str:
    return qualifications.display(tax, q.canonical, q.name_as_written, q.issuing_body, q.jurisdiction)


def _asked(tax: Taxonomy, r: RequiredQualification) -> str:
    """ "US CPA" when the requirement was settled past what the JD wrote, else the JD's word."""
    term = tax.term("certifications", r.canonical or "")
    return term.label if term.variant_of else qualifications.display(tax, r.canonical, r.name_as_written, None, None)


def check_qualification(profile: CandidateProfile, r: RequiredQualification) -> QualificationCheck:
    tax = taxonomy.load()
    required = r.canonical
    assert required is not None
    asked = _asked(tax, r)
    own = tax.acceptable(required, accept_equivalents=False)
    accepted = tax.acceptable(required, r.accept_equivalents)
    level = tax.acceptable(required, accept_equivalents=True)
    # The candidate's best qualification for this requirement: the one asked
    # for, else an accepted equivalent, else one of the right level that the
    # role rules out.
    for ids, passed, reason in (
        (own, True, "Holds {held}, which this role asks for"),
        (accepted, True, f"Holds {{held}}, equivalent to the {asked} this role asks for"),
        (level, False, f"Holds {{held}}, the same level as a {asked}, but this role requires the {asked} itself"),
    ):
        if cid := next((c for c in profile.certifications if c in ids), None):
            return QualificationCheck(required, passed, cid, reason.format(held=_held(tax, profile, cid)))
    held, explanation = _nearest_miss(tax, profile, required, asked, level)
    if explanation is None:
        suffix = " or equivalent" if r.accept_equivalents and len(accepted) > len(own) else ""
        explanation = f"No {asked}{suffix} on the resume"
    return QualificationCheck(required, False, held, explanation)


def _nearest_miss(
    tax: Taxonomy, profile: CandidateProfile, required: str, asked: str, level: tuple[str, ...]
) -> tuple[str | None, str | None]:
    """The requirement is not met: the closest thing the resume does show and
    why it falls short, so ops can see the reason. (None, None) if nothing is close."""
    wanted = tax.term("certifications", required)
    for q in profile.qualifications:
        if q.canonical in level and q.status != "qualified":
            words = f"{_STATUS_WORDS[q.status]} {_shown(tax, q)}"
            return q.canonical, f"{words}: not yet qualified, so it does not meet the {asked} requirement"
    for q in profile.qualifications:
        if q.canonical is None or q.status != "qualified":
            continue
        term = tax.term("certifications", q.canonical)
        if tax.variants(q.canonical) and term.group == wanted.group:
            return q.canonical, (
                f"Holds {_shown(tax, q)} but the resume does not say which body or country awarded it, "
                f"so it cannot be matched to the {asked} this role asks for"
            )
        if term.group is not None and wanted.group is not None and term.group != wanted.group:
            return q.canonical, (
                f"Holds {_shown(tax, q)}, which is a different level ({tax.group_label(term.group).lower()}) "
                f"and not equivalent to the {asked} this role asks for"
            )
    return None, None


def check_experience(profile: CandidateProfile, req: RoleRequirements) -> str | None:
    """None when the candidate has the years the role asks for, else why not."""
    wanted, has = req.min_years_experience, profile.years_experience
    if not wanted or (has is not None and has >= wanted):
        return None
    asks = f"this role asks for at least {wanted}"
    if has is None:
        return f"The resume does not show how many years of experience; {asks}"
    return f"Has {has} {'year' if has == 1 else 'years'} of experience; {asks}"


def check_hard_filters(profile: CandidateProfile, req: RoleRequirements) -> HardFilterResult:
    """Qualifications, software and years of experience: the filters a
    resume can answer. Start date, hours a week and time-zone overlap are
    answered by the candidate, not the profile, and are applied by the API
    (api/internal/availability)."""
    checks = [check_qualification(profile, r) for r in _requirements(req)]
    missing = [s for s in req.required_software if s not in profile.software]
    short = check_experience(profile, req)
    return HardFilterResult(
        passed=all(c.passed for c in checks) and not missing and short is None,
        qualifications=checks,
        missing_software=missing,
        experience_short=short,
    )
