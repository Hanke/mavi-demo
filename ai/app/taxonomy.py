"""Shared canonical vocabulary for the hard-filter fields.

The source of truth is infra/taxonomy.json, which the Go API reads as well
(api/internal/taxonomy). Both sides implement the same `normalize_key` so a
value extracted from a resume here and a value the API writes or seeds land on
the same id. Change the key rules in both places together, and keep
infra/taxonomy_cases.json green on both sides.
"""

from __future__ import annotations

import json
import os
import re
from collections.abc import Mapping
from dataclasses import dataclass
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


class TaxonomyError(ValueError):
    """The taxonomy file is malformed or internally inconsistent."""


class Taxonomy:
    def __init__(self, terms: dict[Kind, list[Term]]):
        self._terms = terms
        self._index: dict[Kind, dict[str, str]] = {kind: _build_index(kind, ts) for kind, ts in terms.items()}
        self._ids: dict[Kind, frozenset[str]] = {kind: frozenset(t.id for t in ts) for kind, ts in terms.items()}

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
                out.append(Term(id=term_id, label=label, aliases=tuple(aliases)))
            terms[kind] = out
        return cls(terms)

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
        for t in self._terms[kind]:
            if t.id == term_id:
                return t
        raise KeyError(f"{kind}: {term_id!r} is not a canonical id")

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
