"""/embed and /embed-batch, the canonical profile and role text, and the
sanity check that a JD lands nearer the profiles it suits."""

from __future__ import annotations

import math
import re
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

from app import embeddings, embedtext, fixtures
from app.cache import Cache
from app.main import MAX_EMBED_INPUTS, MAX_TEXT_CHARS, app
from app.schemas import CandidateProfile, RoleRequirements
from app.settings import Settings, get_settings

MIGRATIONS = Path(__file__).resolve().parents[2] / "infra" / "db" / "migrations"
# The width of every vector(N) column the migrations create.
SCHEMA_DIM = 1536


def test_default_width_is_the_width_of_every_vector_column():
    widths = {int(n) for path in MIGRATIONS.glob("*.up.sql") for n in re.findall(r"vector\((\d+)\)", path.read_text())}
    assert widths == {SCHEMA_DIM}
    assert Settings(_env_file=None).embedding_dim == SCHEMA_DIM  # pyright: ignore[reportCallIssue]


@pytest.fixture
def client():
    """The app with the key-free embedder at its default width, whatever the other test modules overrode."""
    before = app.dependency_overrides.get(get_settings)
    app.dependency_overrides[get_settings] = lambda: Settings(embedding_provider="local", _env_file=None)  # pyright: ignore[reportCallIssue]
    yield TestClient(app)
    if before is None:
        app.dependency_overrides.pop(get_settings, None)
    else:
        app.dependency_overrides[get_settings] = before


def _cosine(a: list[float], b: list[float]) -> float:
    return sum(x * y for x, y in zip(a, b, strict=True)) / (math.hypot(*a) * math.hypot(*b))


# --- canonical text ------------------------------------------------------------


def test_profile_text_is_labelled_lines_with_taxonomy_labels():
    profile = CandidateProfile(
        headline="Senior Accountant",
        years_experience=7,
        positions=[
            {"title": "Senior Accountant", "employer": "Acme"},  # pyright: ignore[reportArgumentType]
            {"title": "Staff Accountant", "employer": "Initech"},  # pyright: ignore[reportArgumentType]
        ],
        certifications=["cpa_us"],
        software=["netsuite", "excel"],
        other_software=["Homegrown ERP"],
        industries=["healthcare"],
        gaap_exposure=["US GAAP", "ASC 606"],
        skills=["month-end close", "Month-end  close", "audit support"],
        timezone="America/Chicago",
        languages=["en"],
    )
    assert embedtext.profile_text(profile).splitlines() == [
        "Role: Senior Accountant; Staff Accountant",
        "Qualifications: US CPA",
        "Software: NetSuite; Microsoft Excel; Homegrown ERP",
        "Industries: Healthcare",
        "Standards: US GAAP; ASC 606",
        "Skills: month-end close; audit support",
    ]


def test_years_are_left_to_the_hard_filter():
    assert embedtext.profile_text(CandidateProfile(headline="Bookkeeper", years_experience=11)) == "Role: Bookkeeper"
    assert embedtext.role_text(RoleRequirements(title="Bookkeeper", min_years_experience=3)) == "Role: Bookkeeper"


def test_role_text_uses_the_same_lines_and_puts_required_before_preferred():
    role = RoleRequirements(
        title="Senior Payroll Specialist",
        required_software=["adp"],
        preferred_software=["workday"],
        other_preferred_software=["UKG"],
        preferred_certifications=["cpp"],
        min_years_experience=1,
        must_haves=["Multi-state payroll"],
        nice_to_haves=["Healthcare payroll background"],
        timezone="America/New_York",
    )
    assert embedtext.role_text(role).splitlines() == [
        "Role: Senior Payroll Specialist",
        "Qualifications: Certified Payroll Professional",
        "Software: ADP; Workday; UKG",
        "Skills: Multi-state payroll; Healthcare payroll background",
    ]


def test_role_text_drops_the_logistics_clauses_of_a_must_have():
    role = RoleRequirements(
        title="Controller",
        must_haves=[
            "Standard costing and inventory valuation under US GAAP",
            "Mountain time; on-site in Denver 3 days a week",
            "Able to start within two weeks",
            "Right to work in the UK; ACCA qualified; based in or commutable to London",
            "Experience managing a remote-first startup's close",  # "remote", but about the work: a known miss
        ],
        nice_to_haves=["Startup experience", "Experience working with a UK parent company"],
    )
    assert embedtext.role_text(role).splitlines() == [
        "Role: Controller",
        "Skills: Standard costing and inventory valuation under US GAAP; ACCA qualified; "
        "Startup experience; Experience working with a UK parent company",
    ]


# Every must-have in the fixtures that is about where, when or what hours.
FIXTURE_LOGISTICS = {
    "bookkeeper_part_time_remote": ["Able to work at least two mornings a week during Pacific time"],
    "controller_manufacturing": ["Willingness to work on-site in Toledo five days a week"],
    "fpa_manager_london": ["Right to work in the UK; based in or commutable to London"],
    "payroll_specialist_remote": [
        "Availability during Eastern Time business hours (8am to 5pm ET), from anywhere in the US"
    ],
    "senior_accountant_strict": [
        "Located in the San Francisco Bay Area and able to work from our SoMa office Tuesday through Thursday; "
        "Pacific time hours",
        "Able to start on or before Monday, November 2, 2026",
    ],
}


@pytest.mark.parametrize("slug", fixtures.jd_slugs())
def test_fixture_jds_lose_exactly_their_logistics(slug: str):
    req = fixtures.load_jd(slug).expected
    dropped = FIXTURE_LOGISTICS.get(slug, [])
    assert set(dropped) <= set(req.must_haves)
    skills = embedtext.role_text(req).splitlines()[-1]
    kept = [m for m in [*req.must_haves, *req.nice_to_haves] if m not in dropped]
    assert skills == "Skills: " + "; ".join(kept)


def test_empty_models_render_to_nothing():
    assert embedtext.profile_text(CandidateProfile()) == ""
    assert embedtext.role_text(RoleRequirements()) == ""


@pytest.mark.parametrize("slug", fixtures.resume_slugs())
def test_every_fixture_profile_renders_the_same_text_twice(slug: str):
    profile = fixtures.load_resume(slug).expected
    text = embedtext.profile_text(profile)
    assert text.startswith("Role: ")
    assert text == embedtext.profile_text(CandidateProfile.model_validate(profile.model_dump()))


# --- /embed-batch --------------------------------------------------------------


def test_batch_returns_one_schema_width_vector_per_input_in_order(client: TestClient):
    resume = fixtures.load_resume("payroll_manager")
    jd = fixtures.load_jd("payroll_specialist_remote")
    body = client.post(
        "/embed-batch",
        json={
            "inputs": [
                {"text": "hello"},
                {"profile": resume.raw["profile"]},
                {"requirements": jd.raw["requirements"]},
                {"text": "hello"},
            ]
        },
    )
    assert body.status_code == 200, body.text
    out = body.json()
    assert (out["dim"], out["provider"]) == (SCHEMA_DIM, "local")
    assert [len(v) for v in out["embeddings"]] == [SCHEMA_DIM] * 4
    assert out["texts"] == [
        "hello",
        embedtext.profile_text(resume.expected),
        embedtext.role_text(jd.expected),
        "hello",
    ]
    assert out["embeddings"][0] == out["embeddings"][3] != out["embeddings"][1]
    # A batch of one is the single endpoint.
    single = client.post("/embed", json={"text": "hello"}).json()
    assert single["dim"] == SCHEMA_DIM
    assert single["embedding"] == out["embeddings"][0]
    for vector in out["embeddings"]:
        assert abs(sum(v * v for v in vector) - 1.0) < 1e-6


def test_single_embed_has_the_same_length_cap_as_a_batch_input(client: TestClient):
    assert client.post("/embed", json={"text": "x" * MAX_TEXT_CHARS}).status_code == 200
    assert client.post("/embed", json={"text": "x" * (MAX_TEXT_CHARS + 1)}).status_code == 422
    assert client.post("/embed-batch", json={"inputs": [{"text": "x" * (MAX_TEXT_CHARS + 1)}]}).status_code == 422


def test_batch_takes_the_whole_seed_in_one_request(client: TestClient):
    inputs = [{"text": f"candidate {i}"} for i in range(MAX_EMBED_INPUTS)]
    out = client.post("/embed-batch", json={"inputs": inputs})
    assert out.status_code == 200
    assert len(out.json()["embeddings"]) == MAX_EMBED_INPUTS
    assert client.post("/embed-batch", json={"inputs": [*inputs, {"text": "one more"}]}).status_code == 422


@pytest.mark.parametrize(
    "inputs",
    [
        [],
        [{}],
        [{"text": ""}],
        [{"text": "a", "profile": {}}],
        [{"profile": {"certifications": ["cpa_us"], "no_such_field": 1}}],
        [{"profile": {}}],  # valid, but renders to no text
        [{"text": " \n "}],
        [{"text": "fine"}, {"requirements": {}}],
    ],
)
def test_batch_rejects_inputs_it_cannot_embed(client: TestClient, inputs: list[dict[str, object]]):
    assert client.post("/embed-batch", json={"inputs": inputs}).status_code == 422


# --- sanity: a JD sits nearer the profiles it suits ----------------------------------


@pytest.mark.parametrize(
    ("jd", "relevant", "irrelevant"),
    [
        ("payroll_specialist_remote", "payroll_manager", "fpa_manager_london"),
        ("payroll_specialist_remote", "payroll_manager", "senior_accountant_cpa_netsuite"),
        ("senior_accountant_strict", "senior_accountant_cpa_netsuite", "payroll_manager"),
        ("senior_accountant_strict", "senior_accountant_cpa_netsuite", "tax_senior_ea"),
        ("fpa_manager_london", "fpa_manager_london", "bookkeeper_part_time"),
        ("bookkeeper_part_time_remote", "bookkeeper_part_time", "controller_manufacturing"),
        ("controller_manufacturing", "controller_manufacturing", "payroll_manager"),
    ],
)
def test_jd_is_closer_to_a_relevant_profile_than_an_irrelevant_one(
    client: TestClient, jd: str, relevant: str, irrelevant: str
):
    out = client.post(
        "/embed-batch",
        json={
            "inputs": [
                {"requirements": fixtures.load_jd(jd).raw["requirements"]},
                {"profile": fixtures.load_resume(relevant).raw["profile"]},
                {"profile": fixtures.load_resume(irrelevant).raw["profile"]},
            ]
        },
    ).json()
    role, good, bad = out["embeddings"]
    assert _cosine(role, good) > _cosine(role, bad) + 0.05


def test_each_fixture_jd_ranks_its_namesake_profile_first():
    """Over all fixture profiles, not one hand-picked pair: the JDs written
    for the same job as a resume put that resume on top."""
    settings = Settings(embedding_provider="local", _env_file=None)  # pyright: ignore[reportCallIssue]
    slugs = fixtures.resume_slugs()
    profiles = embeddings.embed_many(
        [embedtext.profile_text(fixtures.load_resume(s).expected) for s in slugs], settings
    )
    for jd, namesake in {
        "payroll_specialist_remote": "payroll_manager",
        "fpa_manager_london": "fpa_manager_london",
        "controller_manufacturing": "controller_manufacturing",
        "bookkeeper_part_time_remote": "bookkeeper_part_time",
        "senior_accountant_strict": "senior_accountant_cpa_netsuite",
    }.items():
        role = embeddings.embed(embedtext.role_text(fixtures.load_jd(jd).expected), settings)
        best = max(zip(slugs, profiles, strict=True), key=lambda pair: _cosine(role, pair[1]))[0]
        assert best == namesake, jd


# --- provider batching ---------------------------------------------------------


def _openai(tmp_path: Path, **overrides: object) -> Settings:
    return Settings(embedding_provider="openai", embedding_dim=2, ai_cache_dir=str(tmp_path), **overrides)  # pyright: ignore[reportArgumentType]


def test_provider_batch_sends_only_unseen_texts_once_each(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    requests: list[list[str]] = []

    def fake_openai(texts: list[str], settings: Settings) -> list[list[float]]:
        requests.append(texts)
        return [[float(len(t)), 1.0] for t in texts]

    monkeypatch.setattr(embeddings, "_embed_openai", fake_openai)
    settings = _openai(tmp_path)
    cache = Cache(tmp_path)
    embeddings.embed("bb", settings, cache)
    got = embeddings.embed_many(["a", "bb", "ccc", "a"], settings, cache)
    assert got == [[1.0, 1.0], [2.0, 1.0], [3.0, 1.0], [1.0, 1.0]]
    assert requests == [["bb"], ["a", "ccc"]]
    assert (cache.hits, cache.calls) == (1, 3)
    # Everything stored: no request at all.
    embeddings.embed_many(["ccc", "a"], settings, cache)
    assert len(requests) == 2


def test_provider_batch_is_split_into_provider_sized_requests(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    sizes: list[int] = []

    def fake_openai(texts: list[str], settings: Settings) -> list[list[float]]:
        sizes.append(len(texts))
        return [[float(t), 0.0] for t in texts]

    monkeypatch.setattr(embeddings, "_embed_openai", fake_openai)
    monkeypatch.setattr(embeddings, "PROVIDER_BATCH", 4)
    got = embeddings.embed_many([str(i) for i in range(10)], _openai(tmp_path))
    assert sizes == [4, 4, 2]
    assert [v[0] for v in got] == [float(i) for i in range(10)]


def test_a_batch_that_fails_part_way_keeps_what_it_already_paid_for(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    requests: list[list[str]] = []
    down = True

    def fake_openai(texts: list[str], settings: Settings) -> list[list[float]]:
        requests.append(texts)
        if down and "4" in texts:
            raise embeddings.EmbeddingError("rate limited")
        return [[float(t), 0.0] for t in texts]

    monkeypatch.setattr(embeddings, "_embed_openai", fake_openai)
    monkeypatch.setattr(embeddings, "PROVIDER_BATCH", 2)
    settings, texts = _openai(tmp_path), [str(i) for i in range(6)]
    with pytest.raises(embeddings.EmbeddingError):
        embeddings.embed_many(texts, settings)
    down = False
    assert [v[0] for v in embeddings.embed_many(texts, settings)] == [0.0, 1.0, 2.0, 3.0, 4.0, 5.0]
    assert requests == [["0", "1"], ["2", "3"], ["4", "5"], ["4", "5"]]


def test_a_provider_failure_is_a_502_with_the_reason(client: TestClient, tmp_path: Path):
    """No key configured: the real client refuses before any request is made."""
    app.dependency_overrides[get_settings] = lambda: _openai(tmp_path, openai_api_key="")
    resp = client.post("/embed-batch", json={"inputs": [{"text": "a"}]})
    assert resp.status_code == 502
    assert resp.json()["detail"].startswith("embedding provider error: text-embedding-3-small: ")


def test_wrong_width_from_the_provider_is_an_error_not_a_stored_vector(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, client: TestClient
):
    def wide(texts: list[str], settings: Settings) -> list[list[float]]:
        return [[1.0, 2.0, 3.0] for _ in texts]

    monkeypatch.setattr(embeddings, "_embed_openai", wide)
    settings = _openai(tmp_path)
    with pytest.raises(embeddings.EmbeddingError, match="3-dimension"):
        embeddings.embed_many(["a"], settings)
    assert not any(tmp_path.rglob("*.json"))
    app.dependency_overrides[get_settings] = lambda: settings
    for path, body in (("/embed", {"text": "a"}), ("/embed-batch", {"inputs": [{"text": "a"}]})):
        resp = client.post(path, json=body)
        assert resp.status_code == 502
        assert "embedding provider error" in resp.json()["detail"]
