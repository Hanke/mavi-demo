from fastapi.testclient import TestClient

from app.main import app
from app.settings import Settings, get_settings

app.dependency_overrides[get_settings] = lambda: Settings(embedding_provider="local", embedding_dim=8)
client = TestClient(app)


def test_health():
    resp = client.get("/health")
    assert resp.status_code == 200
    assert resp.json()["status"] == "ok"


def test_embed_local_is_deterministic_and_unit_length():
    a = client.post("/embed", json={"text": "hello"}).json()
    b = client.post("/embed", json={"text": "hello"}).json()
    assert a["dim"] == 8
    assert a["embedding"] == b["embedding"]
    assert abs(sum(v * v for v in a["embedding"]) - 1.0) < 1e-6


def test_embed_rejects_empty_text():
    assert client.post("/embed", json={"text": ""}).status_code == 422
