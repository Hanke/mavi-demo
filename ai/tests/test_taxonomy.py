import json
from pathlib import Path

import pytest
from pydantic import ValidationError

from app import taxonomy
from app.schemas import CandidateProfile, RoleRequirements
from app.taxonomy import KINDS, Taxonomy, TaxonomyError, normalize_key

INFRA = Path(__file__).resolve().parents[2] / "infra"
CASES = json.loads((INFRA / "taxonomy_cases.json").read_text())["cases"]


@pytest.fixture(scope="module")
def tax() -> Taxonomy:
    return taxonomy.load()


def test_default_path_is_the_shared_file():
    assert taxonomy.DEFAULT_PATH == INFRA / "taxonomy.json"
    assert taxonomy.DEFAULT_PATH.is_file()


def test_every_kind_has_terms_with_valid_ids(tax: Taxonomy):
    for kind in KINDS:
        ids = tax.ids(kind)
        assert ids, kind
        assert len(set(ids)) == len(ids), f"duplicate ids in {kind}"
        for cid in ids:
            assert tax.resolve(kind, cid) == cid, f"{kind}/{cid} must resolve to itself"
            assert tax.is_canonical(kind, cid)
        for term in tax.terms(kind):
            assert tax.resolve(kind, term.label) == term.id
            for alias in term.aliases:
                assert tax.resolve(kind, alias) == term.id, f"{kind}: alias {alias!r} of {term.id}"


@pytest.mark.parametrize("case", CASES, ids=[f"{c['kind']}:{c['input']!r}" for c in CASES])
def test_shared_alias_cases(tax: Taxonomy, case: dict):
    assert tax.resolve(case["kind"], case["input"]) == case["want"]


@pytest.mark.parametrize(
    ("kind", "raw", "key"),
    [
        ("software", "QuickBooks Online", "quickbooksonline"),
        ("certifications", "  C.P.A.  ", "cpa"),
        ("industries", "Food & Beverage", "foodandbeverage"),
        ("certifications", "CPA (active)", "cpa"),
        ("certifications", "Certified Public Accountant (CPA)", "publicaccountant"),
        ("certifications", "PMP Certified", "pmp"),
        ("certifications", "certified", ""),
        # Credential words are only noise for certifications.
        ("software", "Certified Payroll Suite", "certifiedpayrollsuite"),
        ("industries", "Licensed Cannabis", "licensedcannabis"),
        # A parenthetical glued to a word is part of the name.
        ("industries", "501(c)(3)", "501c3"),
        ("certifications", "CA(SA)", "casa"),
        ("software", "", ""),
        ("software", "---", ""),
    ],
)
def test_normalize_key(kind: str, raw: str, key: str):
    assert normalize_key(kind, raw) == key


def test_resolve_many_splits_and_dedupes(tax: Taxonomy):
    canonical, unknown = tax.resolve_many(
        "software", ["QBO", "QuickBooks Online", "NetSuite", "Zoho Books", " zoho books ", "", "Zoho Books!"]
    )
    assert canonical == ["quickbooks", "netsuite"]
    assert unknown == ["Zoho Books"]


def test_rejects_alias_pointing_at_two_ids():
    data = {
        "certifications": [{"id": "a", "label": "A", "aliases": ["x"]}, {"id": "b", "label": "B", "aliases": ["X"]}],
        "software": [{"id": "s", "label": "S"}],
        "industries": [{"id": "i", "label": "I"}],
    }
    with pytest.raises(TaxonomyError, match="maps to both"):
        Taxonomy.from_dict(data)


def test_rejects_non_snake_case_id():
    data = {
        "certifications": [{"id": "Bad-Id", "label": "A"}],
        "software": [{"id": "s", "label": "S"}],
        "industries": [{"id": "i", "label": "I"}],
    }
    with pytest.raises(TaxonomyError, match="snake_case"):
        Taxonomy.from_dict(data)


# --- Schemas: the resume parser and the JD parser must land on the same ids ---


def test_resume_and_jd_aliases_resolve_to_same_value():
    profile = CandidateProfile(
        certifications=["Certified Public Accountant", "PMP Certified"],
        software=["QuickBooks Online", "Oracle NetSuite ERP", "SFDC"],
        industries=["CPG"],
    )
    role = RoleRequirements(
        required_certifications=["CPA", "Project Management Professional"],
        required_software=["QBO", "Net Suite", "Salesforce.com"],
        industries=["Consumer Packaged Goods"],
    )
    assert profile.certifications == role.required_certifications == ["cpa", "pmp"]
    assert profile.software == role.required_software == ["quickbooks", "netsuite", "salesforce"]
    assert profile.industries == role.industries == ["consumer_packaged_goods"]
    # Containment is what the SQL filter does: p.software @> r.required_software
    assert set(role.required_software) <= set(profile.software)


def test_unknown_values_move_to_other_fields_instead_of_being_dropped():
    profile = CandidateProfile(
        certifications=["CPA", "Series 7", "CPA candidate"],
        software=["Quickbooks", "Zoho Books"],
        other_software=["zoho books", "QBD"],  # a duplicate and a missed alias
    )
    assert profile.certifications == ["cpa"]
    assert profile.other_certifications == ["Series 7", "CPA candidate"]
    assert profile.software == ["quickbooks"]
    assert profile.other_software == ["Zoho Books"]


def test_schema_defaults_are_empty_lists():
    assert CandidateProfile().certifications == []
    assert RoleRequirements().other_required_software == []


def test_item_type_only_accepts_exact_ids():
    # The before-validator normalises whole models, so the per-item constraint
    # is exercised on its own: labels and aliases are not ids.
    from pydantic import TypeAdapter

    from app.schemas import CertificationID, SoftwareID

    with pytest.raises(ValidationError, match="not a canonical"):
        TypeAdapter(SoftwareID).validate_python("Zoho Books")
    with pytest.raises(ValidationError, match="not a canonical"):
        TypeAdapter(CertificationID).validate_python("Certified Public Accountant")
    assert TypeAdapter(SoftwareID).validate_python("bill_com") == "bill_com"


def test_json_schema_carries_taxonomy_enum():
    schema = CandidateProfile.model_json_schema()
    items = schema["properties"]["software"]["items"]
    assert items["type"] == "string"
    assert "quickbooks" in items["enum"] and "netsuite" in items["enum"]
    assert schema["properties"]["other_software"]["items"] == {"type": "string"}
    role = RoleRequirements.model_json_schema()
    assert "cpa" in role["properties"]["required_certifications"]["items"]["enum"]


def test_taxonomy_path_setting_overrides_default(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    from app.settings import Settings, get_settings

    custom = tmp_path / "t.json"
    custom.write_text(
        json.dumps(
            {
                "certifications": [{"id": "zzz", "label": "Zed"}],
                "software": [{"id": "s", "label": "S"}],
                "industries": [{"id": "i", "label": "I"}],
            }
        )
    )
    monkeypatch.setattr("app.settings.get_settings", lambda: Settings(taxonomy_path=str(custom)))
    taxonomy.load.cache_clear()
    try:
        assert taxonomy.load().resolve("certifications", "zed") == "zzz"
    finally:
        monkeypatch.undo()
        taxonomy.load.cache_clear()
    assert taxonomy.load().resolve("certifications", "zed") is None
    assert taxonomy.load().resolve("certifications", "CPA") == "cpa"
