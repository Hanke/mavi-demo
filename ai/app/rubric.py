"""The rerank rubric: what a candidate is scored on, the scale, the weights
and how the overall score is computed.

docs/rerank-rubric.md is the rubric as decided on paper. Its tables are
rendered from this module (`python -m app.rubric`, or `make rubric-render`;
tests/test_rubric.py fails when they are stale), and the /rerank prompt is
built from the same text (app/extract.py), so the document, the prompt and
the arithmetic cannot drift apart. The model only ever gives a level per
dimension; `overall` turns the levels into the score.
"""

from __future__ import annotations

import sys
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Literal

# Stored with a result, so a score can be traced to the weights that produced it. Bump on any change below.
VERSION = "1"

Level = Literal[0, 1, 2, 3, 4]
MAX_LEVEL = 4
# What every level means, whatever the dimension; each dimension then says what it takes to reach it.
LEVEL_NAMES = ("none", "weak", "partial", "strong", "full")

DOC_PATH = Path(__file__).resolve().parents[2] / "docs" / "rerank-rubric.md"
_BEGIN = "<!-- rubric:begin (rendered from ai/app/rubric.py by `make rubric-render`; do not edit) -->"
_END = "<!-- rubric:end -->"


@dataclass(frozen=True)
class Dimension:
    key: str
    label: str
    weight: int
    """Percent of the overall score when every dimension applies."""
    question: str
    levels: tuple[str, str, str, str, str]
    """What a candidate shows at each level, indexed by level (0 first)."""
    null_when: str | None = None
    """When the role gives nothing to score against. None: always scored."""


DIMENSIONS: tuple[Dimension, ...] = (
    Dimension(
        key="must_have_coverage",
        label="Must-have coverage",
        weight=40,
        question=(
            "Does the candidate meet each thing the role states as required: the qualification, the years, "
            "the required software, and any other must-have? Met or not met, not how well."
        ),
        levels=(
            "No must-have is met, or the candidate works in a different field.",
            "Fewer than half of the must-haves are met.",
            "Most must-haves are met, but the required qualification is missing or two or more others are.",
            "All but one must-have is met, and the one missing is not the required qualification: a near miss "
            "(4 years where 5 are asked, a direct counterpart of a required tool) or something the text does "
            "not mention.",
            "Every must-have is met, and the text shows each one.",
        ),
        null_when="the role states no must-haves",
    ),
    Dimension(
        key="experience_depth",
        label="Relevant experience depth",
        weight=25,
        question=(
            "How closely does the work the candidate has actually done match the work of this role: the same "
            "function, at the same seniority and scope, recently, under the same accounting framework?"
        ),
        levels=(
            "No finance or accounting experience.",
            "Finance or accounting experience with little bearing on the work of this role.",
            "An adjacent function (accounts payable for a staff accountant role, audit for a controller role), "
            "or the right function with two or more of the differences listed under level 3.",
            "The same function with one difference: a level more junior or more senior, a clearly smaller "
            "scope, a different accounting framework (IFRS or UK GAAP for a US GAAP role), or not within the "
            "last three years.",
            "Has done this job: the same function and seniority at a comparable scope (entities, team, "
            "volume), within the last three years, under the same accounting framework.",
        ),
    ),
    Dimension(
        key="software_fluency",
        label="Software fluency",
        weight=15,
        question=(
            "How well does the candidate know the systems the role names, required or preferred? Depth of "
            "use, not presence: a product that only appears in a list is listed, not evidenced."
        ),
        levels=(
            "No software is named at all.",
            "Only unrelated finance systems, or only spreadsheets.",
            "The role's main system is only listed with no evidence of use, or the evidenced use is of a "
            "direct counterpart (Sage Intacct for NetSuite, Xero for QuickBooks).",
            "Evidenced hands-on use of the role's main system; the others are only listed, or covered by a "
            "direct counterpart.",
            "Evidenced hands-on use of every system the role names, with specifics: administered, "
            "implemented, migrated, or used for the role's core work.",
        ),
        null_when="the role names no software",
    ),
    Dimension(
        key="industry_fit",
        label="Industry fit",
        weight=10,
        question="Has the candidate worked in the industry the role is in?",
        levels=(
            "Only unrelated industries, or the text does not show any.",
            "General exposure only: mixed clients or industries, none of them the role's or an adjacent one.",
            "An adjacent industry whose accounting works the same way (another subscription business for "
            "SaaS, another inventory business for manufacturing), or clients in the role's industry served "
            "from a practice or an outsourced team.",
            "Has worked in the role's industry, but not in the current or most recent role, or for under three years.",
            "The current or most recent role is in the role's industry, with three or more years in it.",
        ),
        null_when="the role names no industry",
    ),
    Dimension(
        key="nice_to_haves",
        label="Nice-to-haves",
        weight=10,
        question="How many of the things the role states as preferred, a bonus or nice to have does the text show?",
        levels=(
            "None, and nothing close.",
            "None, but something close: studying for a preferred certification, a direct counterpart of a "
            "preferred tool.",
            "At least one, and no more than half.",
            "More than half.",
            "All of them.",
        ),
        null_when="the role states no nice-to-haves",
    ),
)

BY_KEY: dict[str, Dimension] = {d.key: d for d in DIMENSIONS}

# A must-have is not one consideration among five. The overall score of a
# candidate at this level of must_have_coverage is never above the cap,
# however strong the rest is.
MUST_HAVE_CAPS: dict[int, float] = {0: 0.30, 1: 0.50}

SCORE_DECIMALS = 3


def weighted(levels: Mapping[str, int | None]) -> float:
    """The weighted mean of the levels as a fraction of the maximum, before
    the must-have cap. A dimension that is null takes no part, and the
    weights of the rest are scaled up to make 100."""
    total = 0
    weight = 0
    for d in DIMENSIONS:
        level = levels[d.key]
        if level is None:
            if d.null_when is None:
                raise ValueError(f"{d.key} is always scored and cannot be null")
            continue
        if not 0 <= level <= MAX_LEVEL:
            raise ValueError(f"{d.key}: level {level} is not on the 0..{MAX_LEVEL} scale")
        total += d.weight * level
        weight += d.weight
    return total / (MAX_LEVEL * weight)


def overall(levels: Mapping[str, int | None]) -> float:
    """The score of a candidate, 0..1, from their level on each dimension
    (None where the dimension does not apply to the role). The same levels
    always give the same score: this is the only place it is computed."""
    score = weighted(levels)
    must = levels["must_have_coverage"]
    if must is not None and must in MUST_HAVE_CAPS:
        score = min(score, MUST_HAVE_CAPS[must])
    return round(score, SCORE_DECIMALS)


def prompt_text() -> str:
    """The dimensions and level descriptions as the /rerank system prompt
    carries them. The weights are left out: the model is asked for levels, and
    should not be working towards a total."""
    blocks: list[str] = []
    for d in DIMENSIONS:
        lines = [f"{d.key}: {d.question}"]
        lines += [f"  {level}: {d.levels[level]}" for level in range(MAX_LEVEL, -1, -1)]
        lines.append(f"  null: when {d.null_when}." if d.null_when else "  Always scored, never null.")
        blocks.append("\n".join(lines))
    return "\n\n".join(blocks)


def markdown() -> str:
    """The tables of docs/rerank-rubric.md."""
    out = [
        "| Dimension | Key | Weight | Null when |",
        "| --- | --- | --- | --- |",
    ]
    out += [f"| {d.label} | `{d.key}` | {d.weight}% | {d.null_when or 'never: always scored'} |" for d in DIMENSIONS]
    for d in DIMENSIONS:
        out += ["", f"### {d.label} (`{d.key}`, {d.weight}%)", "", d.question, "", "| Level | The text shows |"]
        out.append("| --- | --- |")
        out += [f"| {level} ({LEVEL_NAMES[level]}) | {d.levels[level]} |" for level in range(MAX_LEVEL, -1, -1)]
    return "\n".join(out)


def rendered_doc(current: str) -> str:
    """`current` with the block between the markers replaced by `markdown()`."""
    head, _, rest = current.partition(_BEGIN)
    _, found, tail = rest.partition(_END)
    if not found:
        raise ValueError(f"{DOC_PATH}: rubric markers not found")
    return f"{head}{_BEGIN}\n{markdown()}\n{_END}{tail}"


def main() -> int:
    DOC_PATH.write_text(rendered_doc(DOC_PATH.read_text()))
    print(f"wrote {DOC_PATH}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
