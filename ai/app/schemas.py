"""Structured extraction schemas shared by the resume and JD parsers.

Every hard-filter list (certifications, software, industries) is constrained
to the ids in infra/taxonomy.json. A before-validator resolves whatever the
LLM produced ("QBO", "QuickBooks Online", "Quickbooks") to the canonical id,
and moves anything it cannot resolve into the matching `other_*` free-text
field rather than dropping it. The JSON schema handed to the model carries
the id list as an enum so the model is nudged towards it up front.
"""

from __future__ import annotations

from datetime import date
from typing import Annotated, Any, ClassVar, Literal

from pydantic import AfterValidator, BaseModel, ConfigDict, Field, model_validator

from app import taxonomy
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
        json_schema_extra=lambda schema: schema.update(items={"type": "string", "enum": list(taxonomy.load().ids(kind))}),
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
        data = dict(data)
        tax = taxonomy.load()
        for field, (kind, overflow) in cls.taxonomy_fields.items():
            raw = data.get(field) or []
            extra = data.get(overflow) or []
            if not isinstance(raw, list) or not isinstance(extra, list):
                continue  # let the field validators report the type error
            canonical, unknown = tax.resolve_many(kind, [v for v in raw if isinstance(v, str)])
            # Anything already in the overflow field may itself be an alias the
            # model missed; give it the same chance to resolve.
            more_canonical, still_unknown = tax.resolve_many(kind, [v for v in extra if isinstance(v, str)])
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


class CandidateProfile(TaxonomyModel):
    """What the resume parser extracts. Maps onto candidate_profiles."""

    taxonomy_fields: ClassVar[dict[str, tuple[Kind, str]]] = {
        "certifications": ("certifications", "other_certifications"),
        "software": ("software", "other_software"),
        "industries": ("industries", "other_industries"),
    }

    headline: str | None = Field(default=None, description="One-line summary of the candidate, e.g. 'Senior Accountant'.")
    years_experience: int | None = Field(default=None, ge=0, le=70)
    certifications: list[CertificationID] = _taxonomy_list("certifications", "Certifications the candidate holds, as taxonomy ids.")
    other_certifications: list[str] = Field(default_factory=list, description="Certifications not in the taxonomy, verbatim.")
    software: list[SoftwareID] = _taxonomy_list("software", "Software the candidate has used, as taxonomy ids.")
    other_software: list[str] = Field(default_factory=list, description="Software not in the taxonomy, verbatim.")
    industries: list[IndustryID] = _taxonomy_list("industries", "Industries the candidate has worked in, as taxonomy ids.")
    other_industries: list[str] = Field(default_factory=list, description="Industries not in the taxonomy, verbatim.")
    skills: list[str] = Field(default_factory=list, description="Free-text skills, e.g. 'month-end close'.")
    languages: list[str] = Field(default_factory=list, description="Spoken languages as ISO 639-1 codes.")
    availability: Availability = "unknown"
    available_from: date | None = None
    timezone: str | None = Field(default=None, description="IANA name, e.g. America/Chicago.")


class RoleRequirements(TaxonomyModel):
    """What the JD parser extracts. Maps onto roles."""

    taxonomy_fields: ClassVar[dict[str, tuple[Kind, str]]] = {
        "required_certifications": ("certifications", "other_required_certifications"),
        "required_software": ("software", "other_required_software"),
        "industries": ("industries", "other_industries"),
    }

    title: str | None = None
    required_certifications: list[CertificationID] = _taxonomy_list("certifications", "Certifications the role requires, as taxonomy ids.")
    other_required_certifications: list[str] = Field(default_factory=list, description="Required certifications not in the taxonomy, verbatim.")
    required_software: list[SoftwareID] = _taxonomy_list("software", "Software the role requires, as taxonomy ids.")
    other_required_software: list[str] = Field(default_factory=list, description="Required software not in the taxonomy, verbatim.")
    industries: list[IndustryID] = _taxonomy_list("industries", "Industry context of the role, as taxonomy ids.")
    other_industries: list[str] = Field(default_factory=list, description="Industries not in the taxonomy, verbatim.")
    must_haves: list[str] = Field(default_factory=list, description="Every hard requirement in the JD's own words.")
    nice_to_haves: list[str] = Field(default_factory=list, description="Preferred but not required, in the JD's own words.")
    timezone: str | None = Field(default=None, description="IANA name the role operates in.")
    starts_on: date | None = None
