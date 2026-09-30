"""Shared canonical vocabulary for the hard-filter fields.

The source of truth is infra/taxonomy.json, which the Go API reads as well
(api/internal/taxonomy). Both sides implement the same `normalize_key` so a
value extracted from a resume here and a value the API writes or seeds land on
the same id. Change the key rules in both places together, and keep
infra/taxonomy_cases.json green on both sides.

Accounting qualifications carry extra fields on their certification term: the
equivalence group, the issuing body, the jurisdictions and, for a
body-specific qualification whose letters are ambiguous ("CPA", "CA", "ACA"),
the ambiguous ids it is a variant of. `Taxonomy.acceptable` turns one
required qualification into the ids that satisfy it; the Go side implements
the same rule (taxonomy.Acceptable) and both run the `acceptable` cases in
infra/taxonomy_cases.json.
"""

from __future__ import annotations

import json
import os
import re
from collections.abc import Mapping
from dataclasses import dataclass, replace
from functools import lru_cache
from pathlib import Path
from typing import Literal, cast, get_args

Kind = Literal["certifications", "software", "industries"]
KINDS: tuple[Kind, ...] = get_args(Kind)

# Words that describe holding a credential rather than the credential itself.
# "PMP certified", "CPA license" and "PMP" must all resolve to the same id.
# Only applied to certifications so a product or industry name keeps them.
_CREDENTIAL_WORDS = frozenset(
    {"certified", "certification", "certifications", "certificate", "credential", "license", "licensed", "licence"}
)
# A parenthetical that stands apart from a word is an annotation ("CPA (active)",
# "Certified Public Accountant (CPA)"); one glued to a word is part of the
# name ("501(c)(3)", "CA(SA)").
_PARENTHETICAL = re.compile(r"(^|\s)\([^)]*\)")
_NON_ALNUM = re.compile(r"[^a-z0-9]+")
_ID = re.compile(r"^[a-z0-9]+(_[a-z0-9]+)*$")

DEFAULT_PATH = Path(__file__).resolve().parents[2] / "infra" / "taxonomy.json"


def normalize_key(kind: Kind, raw: str) -> str:
    """Reduce free text to the key used for alias lookup within a kind.

    Mirrors taxonomy.Key in the Go API exactly: lower-case, "&" -> "and", drop
    free-standing parentheticals, drop credential words (certifications only),
    then drop every non-alphanumeric character. Returns "" when nothing is left.
    """
    s = raw.lower().replace("&", " and ")
    s = _PARENTHETICAL.sub(" ", s)
    strip: frozenset[str] = _CREDENTIAL_WORDS if kind == "certifications" else frozenset()
    tokens = [t for t in _NON_ALNUM.split(s) if t and t not in strip]
    return "".join(tokens)


@dataclass(frozen=True)
class Term:
    id: str
    label: str
    aliases: tuple[str, ...]
    # The rest is only set on accounting qualifications (certifications).
    group: str | None = None
    issuing_body: str | None = None
    body_aliases: tuple[str, ...] = ()
    jurisdictions: tuple[str, ...] = ()
    variant_of: tuple[str, ...] = ()
    inherited: tuple[str, ...] = ()
    """Names of the ambiguous terms this one is a variant of: a resume that
    says "CPA" names a US CPA as far as `grounding.mentions` is concerned."""

    @property
    def names(self) -> tuple[str, ...]:
        return (self.label, *self.aliases)

    @property
    def jurisdiction(self) -> str | None:
        """For display: "UK", "Australia / New Zealand", or None."""
        return " / ".join(self.jurisdictions) or None


@dataclass(frozen=True)
class Jurisdiction:
    id: str
    aliases: tuple[str, ...]
    regions: tuple[str, ...]
    """State or province codes, as a location writes them: "Austin, TX"."""
    timezones: tuple[str, ...]


class TaxonomyError(ValueError):
    """The taxonomy file is malformed or internally inconsistent."""


class Taxonomy:
    def __init__(
        self,
        terms: dict[Kind, list[Term]],
        groups: Mapping[str, str] | None = None,
        jurisdictions: list[Jurisdiction] | None = None,
    ):
        self._groups = dict(groups or {})
        self._jurisdictions = list(jurisdictions or [])
        certs = {t.id: t for t in terms.get("certifications", [])}
        for t in certs.values():
            if t.group is not None and t.group not in self._groups:
                raise TaxonomyError(f"taxonomy: certifications/{t.id}: unknown group {t.group!r}")
            for parent in t.variant_of:
                if parent not in certs or parent == t.id:
                    raise TaxonomyError(f"taxonomy: certifications/{t.id}: variant_of {parent!r} is not another id")
                if certs[parent].variant_of:
                    raise TaxonomyError(f"taxonomy: certifications/{t.id}: variant_of {parent!r} is itself a variant")
                if certs[parent].group != t.group:
                    raise TaxonomyError(f"taxonomy: certifications/{t.id}: group differs from {parent!r}")
            for j in t.jurisdictions:
                if j not in {x.id for x in self._jurisdictions}:
                    raise TaxonomyError(f"taxonomy: certifications/{t.id}: unknown jurisdiction {j!r}")
        if "certifications" in terms:
            terms = {
                **terms,
                "certifications": [
                    replace(t, inherited=tuple(n for parent in t.variant_of for n in certs[parent].names))
                    for t in terms["certifications"]
                ],
            }
        self._terms = terms
        self._index: dict[Kind, dict[str, str]] = {kind: _build_index(kind, ts) for kind, ts in terms.items()}
        self._ids: dict[Kind, frozenset[str]] = {kind: frozenset(t.id for t in ts) for kind, ts in terms.items()}
        self._by_id: dict[Kind, dict[str, Term]] = {kind: {t.id: t for t in ts} for kind, ts in terms.items()}

    @classmethod
    def from_dict(cls, data: Mapping[str, object]) -> Taxonomy:
        terms: dict[Kind, list[Term]] = {}
        for kind in KINDS:
            raw_terms = data.get(kind)
            if not isinstance(raw_terms, list) or not raw_terms:
                raise TaxonomyError(f"taxonomy: {kind!r} must be a non-empty list")
            out: list[Term] = []
            for i, t in enumerate(cast(list[object], raw_terms)):
                if not isinstance(t, dict):
                    raise TaxonomyError(f"taxonomy: {kind}[{i}] needs string 'id' and 'label'")
                entry = cast(dict[str, object], t)
                term_id = entry.get("id")
                label = entry.get("label")
                if not isinstance(term_id, str) or not isinstance(label, str):
                    raise TaxonomyError(f"taxonomy: {kind}[{i}] needs string 'id' and 'label'")
                if not _ID.match(term_id):
                    raise TaxonomyError(f"taxonomy: {kind}[{i}] id {term_id!r} must be snake_case ascii")
                raw_aliases = entry.get("aliases", [])
                if not isinstance(raw_aliases, list):
                    raise TaxonomyError(f"taxonomy: {kind}[{i}] aliases must be a list of strings")
                alias_items = cast(list[object], raw_aliases)
                aliases = [a for a in alias_items if isinstance(a, str)]
                if len(aliases) != len(alias_items):
                    raise TaxonomyError(f"taxonomy: {kind}[{i}] aliases must be a list of strings")
                out.append(
                    Term(
                        id=term_id,
                        label=label,
                        aliases=tuple(aliases),
                        group=_opt_str(entry, "group", f"{kind}[{i}]"),
                        issuing_body=_opt_str(entry, "issuing_body", f"{kind}[{i}]"),
                        body_aliases=_str_tuple(entry, "body_aliases", f"{kind}[{i}]"),
                        jurisdictions=_str_tuple(entry, "jurisdictions", f"{kind}[{i}]"),
                        variant_of=_str_tuple(entry, "variant_of", f"{kind}[{i}]"),
                    )
                )
            terms[kind] = out
        groups: dict[str, str] = {}
        raw_groups = data.get("qualification_groups")
        if isinstance(raw_groups, dict):
            for g in cast(list[dict[str, object]], cast(dict[str, object], raw_groups).get("groups", [])):
                groups[cast(str, g["id"])] = cast(str, g["label"])
        jurisdictions: list[Jurisdiction] = []
        raw_jurisdictions = data.get("jurisdictions")
        if isinstance(raw_jurisdictions, list):
            for i, j in enumerate(cast(list[dict[str, object]], raw_jurisdictions)):
                jurisdictions.append(
                    Jurisdiction(
                        id=cast(str, j["id"]),
                        aliases=_str_tuple(j, "aliases", f"jurisdictions[{i}]"),
                        regions=_str_tuple(j, "regions", f"jurisdictions[{i}]"),
                        timezones=_str_tuple(j, "timezones", f"jurisdictions[{i}]"),
                    )
                )
        return cls(terms, groups, jurisdictions)

    @classmethod
    def from_path(cls, path: str | os.PathLike[str]) -> Taxonomy:
        with open(path, encoding="utf-8") as f:
            return cls.from_dict(json.load(f))

    def terms(self, kind: Kind) -> list[Term]:
        return list(self._terms[kind])

    def ids(self, kind: Kind) -> tuple[str, ...]:
        return tuple(t.id for t in self._terms[kind])

    def term(self, kind: Kind, term_id: str) -> Term:
        """The term for a canonical id; KeyError for anything else."""
        try:
            return self._by_id[kind][term_id]
        except KeyError:
            raise KeyError(f"{kind}: {term_id!r} is not a canonical id") from None

    # --- accounting qualifications -------------------------------------------

    def variants(self, term_id: str) -> list[Term]:
        """The body-specific qualifications an ambiguous id ("cpa", "ca", "aca")
        may turn out to be, in taxonomy order; empty for anything else."""
        return [t for t in self._terms["certifications"] if term_id in t.variant_of]

    def group_label(self, group: str) -> str:
        return self._groups[group]

    def acceptable(self, required: str, accept_equivalents: bool) -> tuple[str, ...]:
        """The certification ids that satisfy one required qualification.

        Always the id itself and its variants (a role asking for "a CPA" takes
        any CPA). With equivalents accepted, also every body-specific member
        of the same group. An ambiguous id is never added as a group member:
        a "CPA" from nobody knows where is not evidence of a US CPA equivalent.
        Mirrors taxonomy.Acceptable in the Go API."""
        term = self.term("certifications", required)
        out = [required, *(t.id for t in self.variants(required))]
        if accept_equivalents and term.group is not None:
            for t in self._terms["certifications"]:
                if t.group == term.group and t.id not in out and not self.variants(t.id):
                    out.append(t.id)
        return tuple(out)

    def jurisdictions(self) -> list[Jurisdiction]:
        return list(self._jurisdictions)

    def jurisdiction(self, raw: str | None) -> str | None:
        """A jurisdiction id for free text ("United Kingdom", "england", "USA"), or None."""
        key = _NON_ALNUM.sub("", (raw or "").lower())
        if not key:
            return None
        for j in self._jurisdictions:
            if key in {_NON_ALNUM.sub("", n.lower()) for n in (j.id, *j.aliases)}:
                return j.id
        return None

    def jurisdiction_for_timezone(self, timezone: str | None) -> str | None:
        return next((j.id for j in self._jurisdictions if timezone in j.timezones), None)

    def is_canonical(self, kind: Kind, value: str) -> bool:
        """True only for an exact id, e.g. "bill_com"; aliases and labels are not canonical."""
        return value in self._ids[kind]

    def resolve(self, kind: Kind, raw: str) -> str | None:
        """Map free text to a canonical id, or None when it is not in the taxonomy."""
        key = normalize_key(kind, raw)
        if not key:
            return None
        return self._index[kind].get(key)

    def resolve_many(self, kind: Kind, values: list[str]) -> tuple[list[str], list[str]]:
        """Split values into (canonical ids, unknown free text).

        Both lists are de-duplicated and keep first-seen order. Unknown values
        are returned trimmed but otherwise untouched so the parsers can keep
        them in a free-text field instead of dropping them.
        """
        canonical: list[str] = []
        unknown: list[str] = []
        seen_unknown: set[str] = set()
        for raw in values:
            cid = self.resolve(kind, raw)
            if cid is not None:
                if cid not in canonical:
                    canonical.append(cid)
                continue
            text = raw.strip()
            if not text:
                continue
            k = normalize_key(kind, text)
            if k not in seen_unknown:
                seen_unknown.add(k)
                unknown.append(text)
        return canonical, unknown


def _opt_str(entry: Mapping[str, object], key: str, where: str) -> str | None:
    value = entry.get(key)
    if value is not None and not isinstance(value, str):
        raise TaxonomyError(f"taxonomy: {where} {key} must be a string")
    return value


def _str_tuple(entry: Mapping[str, object], key: str, where: str) -> tuple[str, ...]:
    value = entry.get(key, [])
    if not isinstance(value, list) or not all(isinstance(v, str) for v in cast(list[object], value)):
        raise TaxonomyError(f"taxonomy: {where} {key} must be a list of strings")
    return tuple(cast(list[str], value))


def _build_index(kind: Kind, terms: list[Term]) -> dict[str, str]:
    index: dict[str, str] = {}
    for t in terms:
        for variant in (t.id, t.label, *t.aliases):
            key = normalize_key(kind, variant)
            if not key:
                raise TaxonomyError(f"taxonomy: {kind}/{t.id}: {variant!r} normalises to nothing")
            other = index.get(key)
            if other is not None and other != t.id:
                raise TaxonomyError(f"taxonomy: {kind}: {variant!r} maps to both {other!r} and {t.id!r}")
            index[key] = t.id
    return index


@lru_cache
def load(path: str | None = None) -> Taxonomy:
    """Load the shared taxonomy once. Path precedence: argument, TAXONOMY_PATH setting, repo default."""
    if path is None:
        from app.settings import get_settings

        path = get_settings().taxonomy_path or None
    return Taxonomy.from_path(path or DEFAULT_PATH)
