import pytest


@pytest.fixture(autouse=True)
def isolated_cache(tmp_path_factory: pytest.TempPathFactory, monkeypatch: pytest.MonkeyPatch):
    """No test reads or writes the checkout's ai/.cache: anything that builds a
    cache from settings gets an empty directory of its own."""
    monkeypatch.setenv("AI_CACHE_DIR", str(tmp_path_factory.mktemp("ai-cache")))
    monkeypatch.setenv("AI_CACHE", "on")
