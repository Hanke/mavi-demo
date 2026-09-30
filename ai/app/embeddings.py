import hashlib
import math

from app import cache as cachemod
from app.cache import Cache
from app.settings import Settings


def embed(text: str, settings: Settings, cache: Cache | None = None) -> list[float]:
    """The vector for `text`. A provider's answer is cached on disk under the
    model and the text (app/cache.py); the local stub is already a pure
    function of its input and is not."""
    if settings.embedding_provider == "openai":
        cache = cache or cachemod.from_settings(settings)
        key = cachemod.key("embedding", provider="openai", model=settings.embedding_model, input=text)
        return cache.get_or_call(
            key,
            lambda: _embed_openai(text, settings),
            kind="embedding",
            provider="openai",
            model=settings.embedding_model,
        )
    return _embed_local(text, settings.embedding_dim)


def _embed_openai(text: str, settings: Settings) -> list[float]:
    from openai import OpenAI

    client = OpenAI(api_key=settings.openai_api_key)
    resp = client.embeddings.create(model=settings.embedding_model, input=text)
    return resp.data[0].embedding


def _embed_local(text: str, dim: int) -> list[float]:
    """Deterministic, key-free stand-in so the stack runs without provider credentials.

    Hashes the text into a unit vector of the configured width. Not semantically
    meaningful, but stable across runs, which is all the plumbing needs.
    """
    seed = hashlib.sha256(text.encode("utf-8")).digest()
    values: list[float] = []
    counter = 0
    while len(values) < dim:
        block = hashlib.sha256(seed + counter.to_bytes(4, "big")).digest()
        for i in range(0, len(block), 2):
            if len(values) == dim:
                break
            raw = int.from_bytes(block[i : i + 2], "big")
            values.append(raw / 32767.5 - 1.0)
        counter += 1
    norm = math.sqrt(sum(v * v for v in values)) or 1.0
    return [v / norm for v in values]
