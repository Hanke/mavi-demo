"""Structured extraction schemas shared by the resume and JD parsers.

Every hard-filter list (certifications, software, industries) is constrained
to the ids in infra/taxonomy.json. A before-validator resolves whatever the
LLM produced ("QBO", "QuickBooks Online", "Quickbooks") to the canonical id,
and moves anything it cannot resolve into the matching `other_*` free-text
field rather than dropping it. The JSON schema handed to the model carries
the id list as an enum so the model is nudged towards it up front.

Accounting qualifications get a fuller record next to the id list
(`qualifications`, `required_qualifications`): what was written, which body
and jurisdiction, and whether it is actually held. The id lists stay the
hard-filter columns and are kept in step with those records by the validators
here, so "part-qualified ACCA" never puts `acca` in `certifications`.
"""

from __future__ import annotations

from datetime import date
from typing import Annotated, Any, ClassVar, Literal, cast

from pydantic import AfterValidator, BaseModel, ConfigDict, Field, field_validator, model_validator

from app import qualifications, taxonomy
from app.qualifications import Status
from app.taxonomy import Kind

Availability = Literal["immediate", "two_weeks", "one_month", "unavailable", "unknown"]


def _canonical(kind: Kind):
    def check(value: str) -> str:
        if not taxonomy.load().is_canonical(kind, value):
            raise ValueError(f"{value!r} is not a canonical {kind} id")
        return value

    return check


CertificationID = Annotated[str, AfterValidator(_canonical("certifications"))]
SoftwareID = Annotated[str, AfterValidator(_canonical("software"))]
IndustryID = Annotated[str, AfterValidator(_canonical("industries"))]


def _taxonomy_list(kind: Kind, description: str) -> Any:
    return Field(
        default_factory=list,
        description=description,
        json_schema_extra=lambda schema: schema.update(
            items={"type": "string", "enum": list(taxonomy.load().ids(kind))}
        ),
    )


class TaxonomyModel(BaseModel):
    """Base for models whose list fields are constrained to the taxonomy.

    Subclasses declare `taxonomy_fields` as {field: (kind, overflow_field)}.
    """

    model_config = ConfigDict(extra="forbid")
    taxonomy_fields: ClassVar[dict[str, tuple[Kind, str]]] = {}

    @model_validator(mode="before")
    @classmethod
    def _resolve_taxonomy_fields(cls, data: Any) -> Any:
        if not isinstance(data, dict):
            return data
        data = dict(cast(dict[str, Any], data))
        tax = taxonomy.load()
        for field, (kind, overflow) in cls.taxonomy_fields.items():
            raw: object = data.get(field) or []
            extra: object = data.get(overflow) or []
            if not isinstance(raw, list) or not isinstance(extra, list):
                continue  # let the field validators report the type error
            raw_items = cast(list[object], raw)
            extra_items = cast(list[object], extra)
            canonical, unknown = tax.resolve_many(kind, [v for v in raw_items if isinstance(v, str)])
            # Anything already in the overflow field may itself be an alias the
            # model missed; give it the same chance to resolve.
            more_canonical, still_unknown = tax.resolve_many(kind, [v for v in extra_items if isinstance(v, str)])
            for cid in more_canonical:
                if cid not in canonical:
                    canonical.append(cid)
            merged_unknown = list(unknown)
            seen = {taxonomy.normalize_key(kind, u) for u in merged_unknown}
            for text in still_unknown:
                if taxonomy.normalize_key(kind, text) not in seen:
                    seen.add(taxonomy.normalize_key(kind, text))
                    merged_unknown.append(text)
            data[field] = canonical
            data[overflow] = merged_unknown
        return data


class Contact(BaseModel):
    """Header details the resume parser returns alongside the profile.

    Every field is nullable: a resume with no email gets null, not a made-up
    address, and a blank string from the model means the same thing."""

    model_config = ConfigDict(extra="forbid")

    full_name: str | None = None
    email: str | None = None
    phone: str | None = None
    location: str | None = None

    @field_validator("*", mode="before")
    @classmethod
    def _blank_is_absent(cls, value: object) -> object:
        if isinstance(value, str):
            return value.strip() or None
        return value


class Position(BaseModel):
    """One job in the candidate's work history, as the resume lists it."""

    model_config = ConfigDict(extra="forbid")

    title: str = Field(min_length=1, description="Job title as written, e.g. 'Senior Accountant'.")
    employer: str | None = Field(default=None, description="Employer as written; null if the resume names none.")
    start_year: int | None = Field(default=None, ge=1950, le=2100, description="Four-digit year the role began.")
    end_year: int | None = Field(
        default=None, ge=1950, le=2100, description="Four-digit year the role ended; null for a current role."
    )
    current: bool = Field(default=False, description="True when the resume says the role runs to the present.")

    @model_validator(mode="after")
    def _years_are_ordered(self) -> Position:
        if self.current:
            self.end_year = None
        if self.start_year is not None and self.end_year is not None and self.end_year < self.start_year:
            raise ValueError(f"end_year {self.end_year} is before start_year {self.start_year}")
        return self


def _certification_id(description: str) -> Any:
    return Field(
        default=None,
        description=description,
        json_schema_extra=lambda schema: schema.update(
            anyOf=[{"type": "string", "enum": list(taxonomy.load().ids("certifications"))}, {"type": "null"}]
        ),
    )


def _blank_is_none(value: object) -> object:
    if isinstance(value, str):
        return value.strip() or None
    return value


class Qualification(BaseModel):
    """One professional qualification exactly as the resume gives it.

    `canonical` is the qualification the candidate holds, never the one a
    role might want instead: an ACA is `aca_icaew`, not `cpa`."""

    model_config = ConfigDict(extra="forbid")

    name_as_written: str = Field(min_length=1, description="Exactly what the resume says, e.g. 'ACA', 'CPA, CA'.")
    canonical: str | None = _certification_id(
        "Taxonomy id of this qualification itself, e.g. 'aca_icaew'; null if it is not in the taxonomy."
    )
    issuing_body: str | None = Field(default=None, description="e.g. 'ICAEW', 'ACCA', 'CPA Ontario'; null if unstated.")
    jurisdiction: str | None = Field(
        default=None, description="Where it was awarded, e.g. 'UK', 'US', 'Australia'; null if it cannot be determined."
    )
    status: Status = Field(
        default="qualified",
        description="qualified only when fully held; 'ACCA finalist' is part_qualified, 'studying for' in_progress.",
    )
    year_obtained: int | None = Field(default=None, ge=1950, le=2100)
    quote: str | None = Field(default=None, description="The resume's own words that show it, verbatim.")

    _blank = field_validator("name_as_written", "canonical", "issuing_body", "jurisdiction", "quote", mode="before")(
        _blank_is_none
    )

    def settled(self, hint: str | None) -> Qualification:
        tax = taxonomy.load()
        canonical, body, where = qualifications.settle(
            tax, self.name_as_written, self.canonical, self.issuing_body, self.jurisdiction, hint=hint
        )
        status = qualifications.status_from(self.name_as_written, self.quote, self.status)
        return self.model_copy(
            update={"canonical": canonical, "issuing_body": body, "jurisdiction": where, "status": status}
        )


class RequiredQualification(BaseModel):
    """One qualification a role requires, and whether an equivalent will do."""

    model_config = ConfigDict(extra="forbid")

    name_as_written: str = Field(min_length=1, description="The qualification as the JD names it, e.g. 'CPA'.")
    canonical: str | None = _certification_id(
        "Taxonomy id of the qualification asked for; null if not in the taxonomy."
    )
    accept_equivalents: bool = Field(
        default=True,
        description="False only when the JD rules equivalents out, e.g. 'active US CPA licence required'.",
    )
    equivalents_stated: bool = Field(
        default=False,
        description="True when the JD says either way ('CPA or equivalent', 'US licence required'); "
        "false means accept_equivalents is the default and the employer should confirm it at intake.",
    )
    quote: str | None = Field(default=None, description="The JD's own words, verbatim.")

    _blank = field_validator("name_as_written", "canonical", "quote", mode="before")(_blank_is_none)


def _sync_ids(
    ids: list[str], other: list[str], records: list[tuple[str | None, str, bool]]
) -> tuple[list[str], list[str]]:
    """Bring an id list (and its free-text overflow) in step with the fuller
    records: (canonical, name as written, counts). A record replaces whatever
    the list said about the same family of letters, so a bare "cpa" next to a
    record settled as `cpa_us` becomes `cpa_us`, and one that does not count
    (part-qualified) takes its id out."""
    tax = taxonomy.load()
    for canonical, name, counts in records:
        related = {qualifications.resolve_name(tax, name), canonical} - {None}
        for term_id in list(related):
            related |= set(tax.term("certifications", cast(str, term_id)).variant_of)
        # The record takes the place of the first id it supersedes, so the list keeps its order.
        at = next((i for i, cid in enumerate(ids) if cid in related), len(ids))
        ids = [cid for cid in ids if cid not in related]
        key = taxonomy.normalize_key("certifications", name)
        other = [o for o in other if taxonomy.normalize_key("certifications", o) != key]
        if not counts:
            continue
        if canonical is not None and canonical not in ids:
            ids.insert(min(at, len(ids)), canonical)
        elif canonical is None:
            other.append(name)
    return ids, other


def _unique[Q: (Qualification, RequiredQualification)](records: list[Q]) -> list[Q]:
    """One record per qualification: the first for each id (or unresolved name) wins."""
    seen: set[str] = set()
    out: list[Q] = []
    for r in records:
        key = r.canonical or "?" + taxonomy.normalize_key("certifications", r.name_as_written)
        if key not in seen:
            seen.add(key)
            out.append(r)
    return out


class CandidateProfile(TaxonomyModel):
    """What the resume parser extracts. Maps onto candidate_profiles."""

    taxonomy_fields: ClassVar[dict[str, tuple[Kind, str]]] = {
        "certifications": ("certifications", "other_certifications"),
        "software": ("software", "other_software"),
        "industries": ("industries", "other_industries"),
    }

    headline: str | None = Field(
        default=None, description="One-line summary of the candidate, e.g. 'Senior Accountant'."
    )
    years_experience: int | None = Field(default=None, ge=0, le=70)
    positions: list[Position] = Field(
        default_factory=list[Position], description="Jobs held, in the order the resume lists them."
    )
    certifications: list[CertificationID] = _taxonomy_list(
        "certifications", "Certifications the candidate holds, as taxonomy ids."
    )
    other_certifications: list[str] = Field(
        default_factory=list, description="Certifications not in the taxonomy, verbatim."
    )
    qualifications: list[Qualification] = Field(
        default_factory=list[Qualification],
        description="Every professional qualification the resume mentions, held or not, as written.",
    )
    software: list[SoftwareID] = _taxonomy_list("software", "Software the candidate has used, as taxonomy ids.")
    other_software: list[str] = Field(default_factory=list, description="Software not in the taxonomy, verbatim.")
    industries: list[IndustryID] = _taxonomy_list(
        "industries", "Industries the candidate has worked in, as taxonomy ids."
    )
    other_industries: list[str] = Field(default_factory=list, description="Industries not in the taxonomy, verbatim.")
    gaap_exposure: list[str] = Field(
        default_factory=list,
        description="Accounting frameworks and standards the resume names, e.g. 'US GAAP', 'ASC 606', 'IFRS 17'.",
    )
    skills: list[str] = Field(default_factory=list, description="Free-text skills, e.g. 'month-end close'.")
    languages: list[str] = Field(default_factory=list, description="Spoken languages as ISO 639-1 codes.")
    availability: Availability = "unknown"
    available_from: date | None = None
    timezone: str | None = Field(default=None, description="IANA name, e.g. America/Chicago.")

    @model_validator(mode="after")
    def _settle_qualifications(self) -> CandidateProfile:
        """Fix each qualification's id, body and jurisdiction (the candidate's
        time zone stands in for a location when nothing else says where a
        "CPA" is from), then make `certifications` hold exactly the ones that
        are fully held."""
        if not self.qualifications:
            return self
        hint = taxonomy.load().jurisdiction_for_timezone(self.timezone)
        settled = [q.settled(hint) for q in self.qualifications]
        records = _unique(settled)
        # An id the model gave a qualification that turned out to be something
        # else (cpa for an ACA) goes, unless another record really is that.
        overruled = {q.canonical for q in self.qualifications} - {q.canonical for q in settled}
        ids, other = _sync_ids(
            [c for c in self.certifications if c not in overruled],
            list(self.other_certifications),
            [(q.canonical, q.name_as_written, q.status == "qualified") for q in records],
        )
        self.qualifications, self.certifications, self.other_certifications = records, ids, other
        return self


class RoleRequirements(TaxonomyModel):
    """What the JD parser extracts. Maps onto roles."""

    taxonomy_fields: ClassVar[dict[str, tuple[Kind, str]]] = {
        "required_certifications": ("certifications", "other_required_certifications"),
        "required_software": ("software", "other_required_software"),
        "industries": ("industries", "other_industries"),
    }

    title: str | None = None
    required_certifications: list[CertificationID] = _taxonomy_list(
        "certifications", "Certifications the role requires, as taxonomy ids."
    )
    other_required_certifications: list[str] = Field(
        default_factory=list, description="Required certifications not in the taxonomy, verbatim."
    )
    required_qualifications: list[RequiredQualification] = Field(
        default_factory=list[RequiredQualification],
        description="Each required qualification with whether an equivalent is acceptable.",
    )
    required_software: list[SoftwareID] = _taxonomy_list("software", "Software the role requires, as taxonomy ids.")
    other_required_software: list[str] = Field(
        default_factory=list, description="Required software not in the taxonomy, verbatim."
    )
    industries: list[IndustryID] = _taxonomy_list("industries", "Industry context of the role, as taxonomy ids.")
    other_industries: list[str] = Field(default_factory=list, description="Industries not in the taxonomy, verbatim.")
    must_haves: list[str] = Field(default_factory=list, description="Every hard requirement in the JD's own words.")
    nice_to_haves: list[str] = Field(
        default_factory=list, description="Preferred but not required, in the JD's own words."
    )
    timezone: str | None = Field(default=None, description="IANA name the role operates in.")
    starts_on: date | None = None

    @model_validator(mode="after")
    def _settle_qualifications(self) -> RoleRequirements:
        """As on the candidate side. Which "CPA" a JD means comes from its own
        words ("any US state") and otherwise from where the role is."""
        if not self.required_qualifications:
            return self
        tax = taxonomy.load()
        records: list[RequiredQualification] = []
        for r in self.required_qualifications:
            stated = qualifications.guess_jurisdiction(tax, f"{r.name_as_written} {r.quote or ''}")
            hint = stated or tax.jurisdiction_for_timezone(self.timezone)
            canonical, _, _ = qualifications.settle(tax, r.name_as_written, r.canonical, None, None, hint=hint)
            records.append(r.model_copy(update={"canonical": canonical}))
        records = _unique(records)
        ids, other = _sync_ids(
            list(self.required_certifications),
            list(self.other_required_certifications),
            [(r.canonical, r.name_as_written, True) for r in records],
        )
        self.required_qualifications, self.required_certifications, self.other_required_certifications = (
            records,
            ids,
            other,
        )
        return self
