"""ai/openapi.json is the contract the Go API generates its client types from."""

import json

from app import openapi


def test_committed_spec_matches_the_app():
    committed = json.loads(openapi.SPEC_PATH.read_text())
    assert committed == openapi.build(), (
        "ai/openapi.json is stale: a request or response model changed. "
        "Run `make generate` (or `python -m app.openapi` in ai/) and commit the result."
    )


def test_dump_is_stable():
    assert openapi.dump() == openapi.dump()
    assert openapi.dump().endswith("\n")


def test_shared_parser_models_are_exported_without_taxonomy_enums():
    schemas = openapi.build()["components"]["schemas"]
    for name, fields in {
        "CandidateProfile": ("certifications", "software", "industries"),
        "RoleRequirements": ("required_certifications", "required_software", "industries"),
    }.items():
        for field in fields:
            items = schemas[name]["properties"][field]["items"]
            assert items["type"] == "string"
            assert "enum" not in items, f"{name}.{field} must not bake taxonomy ids into the contract"


def test_only_non_nullable_arrays_skip_the_go_pointer():
    schemas = openapi.build()["components"]["schemas"]
    profile = schemas["CandidateProfile"]["properties"]
    assert profile["skills"]["x-go-type-skip-optional-pointer"] is True
    assert "x-go-type-skip-optional-pointer" not in profile["available_from"]
    assert profile["available_from"]["nullable"] is True


def test_endpoints_the_go_client_calls_are_present():
    paths = openapi.build()["paths"]
    assert set(paths) >= {"/health", "/embed"}
    assert paths["/embed"]["post"]["requestBody"]["content"]["application/json"]["schema"]["$ref"].endswith(
        "/EmbedRequest"
    )
