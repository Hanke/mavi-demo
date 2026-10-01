"""The rerank rubric: the document, the prompt, the schema and the arithmetic
all follow app/rubric.py."""

from __future__ import annotations

import pytest

from app import llm, rubric
from app.extract import RERANK_SYSTEM, DimensionScores, RerankOutput


def _levels(must: int | None, experience: int, software: int | None, industry: int | None, nice: int | None):
    return {
        "must_have_coverage": must,
        "experience_depth": experience,
        "software_fluency": software,
        "industry_fit": industry,
        "nice_to_haves": nice,
    }


def test_weights_add_up_and_every_level_is_described():
    assert sum(d.weight for d in rubric.DIMENSIONS) == 100
    for d in rubric.DIMENSIONS:
        assert len(d.levels) == rubric.MAX_LEVEL + 1
        assert all(text.strip() for text in d.levels)
    assert len(rubric.LEVEL_NAMES) == rubric.MAX_LEVEL + 1


def test_committed_document_matches_the_rubric():
    current = rubric.DOC_PATH.read_text()
    assert current == rubric.rendered_doc(current), (
        "docs/rerank-rubric.md is stale: app/rubric.py changed. Run `make rubric-render` and commit the result."
    )
    assert f"Version {rubric.VERSION}." in current


def test_prompt_carries_every_dimension_and_level_description():
    for d in rubric.DIMENSIONS:
        assert f"{d.key}: {d.question}" in RERANK_SYSTEM
        for level, text in enumerate(d.levels):
            assert f"  {level}: {text}" in RERANK_SYSTEM
        if d.null_when:
            assert f"null: when {d.null_when}." in RERANK_SYSTEM


def test_schema_has_one_field_per_dimension_and_no_overall_score():
    assert list(DimensionScores.model_fields) == [d.key for d in rubric.DIMENSIONS]
    schema = llm.output_schema(RerankOutput)
    defs = schema["$defs"]
    assert "score" not in defs["RerankJudgement"]["properties"]
    assert defs["DimensionScore"]["properties"]["level"]["enum"] == [0, 1, 2, 3, 4]
    for d in rubric.DIMENSIONS:
        ref = defs["DimensionScores"]["properties"][d.key]["$ref"].rsplit("/", 1)[-1]
        assert ref == ("OptionalDimensionScore" if d.null_when else "DimensionScore")
    optional = defs["OptionalDimensionScore"]["properties"]["level"]["anyOf"]
    assert {"type": "null"} in optional


@pytest.mark.parametrize(
    ("levels", "score"),
    [
        (_levels(4, 4, 4, 4, 4), 1.0),
        (_levels(0, 0, 0, 0, 0), 0.0),
        (_levels(3, 3, 2, 4, None), 0.736),  # the worked examples in docs/rerank-rubric.md
        (_levels(1, 4, 4, 4, 4), 0.5),
        (_levels(0, 4, 4, 4, 4), 0.3),
        (_levels(2, 4, 4, 4, 4), 0.8),  # no cap from level 2
        (_levels(None, 2, None, 3, None), 0.571),
        (_levels(1, 0, 0, 0, 0), 0.1),  # under the cap: the cap is a ceiling, not a floor
    ],
)
def test_overall_score(levels: dict[str, int | None], score: float):
    assert rubric.overall(levels) == score


def test_a_higher_level_never_lowers_the_score():
    base = _levels(2, 2, 2, 2, 2)
    for key in base:
        for level in range(rubric.MAX_LEVEL):
            lower, higher = base | {key: level}, base | {key: level + 1}
            assert rubric.overall(higher) >= rubric.overall(lower)


def test_levels_off_the_scale_are_refused():
    with pytest.raises(ValueError, match="always scored"):
        rubric.overall(_levels(4, None, 4, 4, 4))  # pyright: ignore[reportArgumentType]
    with pytest.raises(ValueError, match="scale"):
        rubric.overall(_levels(5, 4, 4, 4, 4))
    with pytest.raises(KeyError):
        rubric.overall({"experience_depth": 4})
