"""The AI service's OpenAPI document, as committed to ai/openapi.json.

That file is the contract for every Go -> Python call: the Go API generates
its client types from it (api/internal/aiclient/types.gen.go), so a change to
a request or response model here has to be exported (`make generate`) before
the Go side compiles against it. tests/test_openapi.py fails when the
committed file no longer matches what the app serves.

Three adjustments are made to what FastAPI serves at /openapi.json:

* The document is downgraded from OpenAPI 3.1 to 3.0.3, which is what
  oapi-codegen reads: an optional field's `anyOf: [X, {"type": "null"}]`
  becomes X with `nullable: true`, and `const` becomes a one-value `enum`.
  Nothing the service exposes needs a 3.1-only construct.

* The parser output models (CandidateProfile, RoleRequirements) are added as
  components even though no endpoint returns them yet. They are the shapes
  the API stores in `candidate_profiles.profile` and `roles.requirements`,
  so consumers can generate types for them from one place.
* The taxonomy id enums are removed from those models' list fields and from
  the `canonical` of a qualification record. The ids
  come from infra/taxonomy.json, which is mounted at runtime and edited
  without a rebuild; baking them into the contract would make every
  taxonomy edit a schema change. They stay validated at runtime.
* Every non-nullable array property gets `x-go-type-skip-optional-pointer`,
  so oapi-codegen emits `[]string` rather than `*[]string` for the list
  fields (they default to an empty list and are never null). Nullable
  fields keep their pointer so a JSON null still decodes.
"""

# pyright: reportUnknownVariableType=false, reportUnknownMemberType=false, reportUnknownArgumentType=false
# This module walks an untyped JSON document in place, so the strict-mode
# "unknown type" rules would only be satisfied by casts at every step. The
# other strict rules stay on.
from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from pydantic.json_schema import models_json_schema

from app.main import app
from app.schemas import CandidateProfile, RoleRequirements, TaxonomyModel

SPEC_PATH = Path(__file__).resolve().parents[1] / "openapi.json"

SHARED_MODELS: tuple[type[TaxonomyModel], ...] = (CandidateProfile, RoleRequirements)
# Their `canonical` is a certification id too, with the same enum to strip.
QUALIFICATION_MODELS = ("Qualification", "RequiredQualification")


def build() -> dict[str, Any]:
    """Return the OpenAPI document as a plain, JSON-serialisable dict."""
    spec: dict[str, Any] = json.loads(json.dumps(app.openapi()))
    _, definitions = models_json_schema(
        [(model, "validation") for model in SHARED_MODELS],
        ref_template="#/components/schemas/{model}",
    )
    schemas: dict[str, Any] = spec.setdefault("components", {}).setdefault("schemas", {})
    schemas.update(definitions.get("$defs", {}))
    for model in SHARED_MODELS:
        properties = schemas[model.__name__]["properties"]
        for field in model.taxonomy_fields:
            items = properties[field]["items"]
            items.pop("enum", None)
            items["description"] = "A canonical id from infra/taxonomy.json; validated at runtime."
    for name in QUALIFICATION_MODELS:
        for variant in schemas[name]["properties"]["canonical"]["anyOf"]:
            variant.pop("enum", None)
    spec["openapi"] = "3.0.3"
    _downgrade(spec)
    _plain_go_slices(spec)
    return spec


def _plain_go_slices(node: Any) -> None:
    """Mark non-nullable array properties so the Go generator skips the optional pointer."""
    if isinstance(node, list):
        for item in node:
            _plain_go_slices(item)
        return
    if not isinstance(node, dict):
        return
    for prop in node.get("properties", {}).values():
        if isinstance(prop, dict) and prop.get("type") == "array" and not prop.get("nullable"):
            prop["x-go-type-skip-optional-pointer"] = True
    for value in node.values():
        _plain_go_slices(value)


def _downgrade(node: Any) -> None:
    """Rewrite 3.1 schema constructs into their 3.0 equivalents, in place."""
    if isinstance(node, list):
        for item in node:
            _downgrade(item)
        return
    if not isinstance(node, dict):
        return
    for value in node.values():
        _downgrade(value)
    if "const" in node:
        node["enum"] = [node.pop("const")]
    variants = node.get("anyOf")
    if isinstance(variants, list) and {"type": "null"} in variants:
        others = [v for v in variants if v != {"type": "null"}]
        if len(others) == 1:
            del node["anyOf"]
            only = others[0]
            if "$ref" in only:
                # 3.0 allows no siblings next to $ref.
                node["allOf"] = [only]
            else:
                node.update(only)
        else:
            node["anyOf"] = others
        node["nullable"] = True


def dump() -> str:
    """The document as stable text: sorted keys, two-space indent, trailing newline."""
    return json.dumps(build(), indent=2, sort_keys=True) + "\n"


def main() -> None:
    SPEC_PATH.write_text(dump())
    print(f"wrote {SPEC_PATH}")


if __name__ == "__main__":
    main()
