import hashlib
import math
import re
from itertools import pairwise

from app import cache as cachemod
from app.cache import Cache
from app.settings import Settings

# Texts sent to the provider in one request. OpenAI takes up to 2048 inputs
# but caps the tokens of a whole request, and one input may be a full resume.
PROVIDER_BATCH = 64


class EmbeddingError(Exception):
    """The provider failed, or its answer cannot be stored: wrong count or wrong width."""


def embed(text: str, settings: Settings, cache: Cache | None = None) -> list[float]:
    """The vector for `text`; see `embed_many`."""
    return embed_many([text], settings, cache)[0]


def embed_many(texts: list[str], settings: Settings, cache: Cache | None = None) -> list[list[float]]:
    """One vector per text, in order, each `settings.embedding_dim` wide.

    A provider's answer is cached on disk under the model, the width and the
    text (app/cache.py), so only the texts not seen before are sent, a
    repeated text is sent once, and they go in as few requests as
    PROVIDER_BATCH allows. Each request's vectors are stored when it
    returns, so a batch that fails part-way is not paid for twice. The local
    embedder is already a pure function of its input and is not cached."""
    dim = settings.embedding_dim
    if settings.embedding_provider != "openai":
        return [_embed_local(text, dim) for text in texts]

    cache = cache or cachemod.from_settings(settings)
    model = settings.embedding_model
    keys = [cachemod.key("embedding", provider="openai", model=model, dim=dim, input=text) for text in texts]
    text_for = dict(zip(keys, texts, strict=True))

    def call(missing: list[str]) -> list[list[float]]:
        got = _embed_openai([text_for[k] for k in missing], settings)
        if len(got) != len(missing):
            raise EmbeddingError(f"{model} returned {len(got)} embeddings for {len(missing)} texts")
        for vector in got:
            if len(vector) != dim:
                raise EmbeddingError(f"{model} returned a {len(vector)}-dimension embedding; EMBEDDING_DIM is {dim}")
        return got

    return cache.get_or_call_many(keys, call, batch=PROVIDER_BATCH, kind="embedding", provider="openai", model=model)


def _embed_openai(texts: list[str], settings: Settings) -> list[list[float]]:
    from openai import OpenAI, OpenAIError

    model = settings.embedding_model
    try:
        # Constructing the client is what rejects a missing key.
        client = OpenAI(api_key=settings.openai_api_key)
        if model.startswith("text-embedding-3"):
            # These models can shorten their output to the width asked for,
            # so a larger model still fits the vector columns.
            resp = client.embeddings.create(model=model, input=texts, dimensions=settings.embedding_dim)
        else:
            resp = client.embeddings.create(model=model, input=texts)
    except OpenAIError as e:
        raise EmbeddingError(f"{model}: {e}") from e
    return [item.embedding for item in sorted(resp.data, key=lambda item: item.index)]


_WORD = re.compile(r"[^\W_]+")
# Where one item ends and the next begins: a line, a list entry, a sentence.
# A pair of words is only a term when both are on the same side of one.
_BREAK = re.compile(r"[\n;:,()]|\.\s")
# Words that say nothing about fit, plus the line labels of app/embedtext.py
# that every rendered document shares. No taxonomy label uses one of them.
_STOP_WORDS = frozenset(
    {"a", "an", "and", "are", "as", "at", "be", "by", "for", "from", "in", "is", "it", "of", "on", "or", "that"}
    | {"the", "to", "with", "year", "years", "experience"}
    | {"role", "qualifications", "software", "industries", "standards", "skills"}
)


def _embed_local(text: str, dim: int) -> list[float]:
    """Deterministic, key-free stand-in so the stack runs without provider credentials.

    A hashed bag of words: every word and every pair of neighbouring words
    (within one list entry or sentence) is hashed to one of the `dim` components (with a hashed sign, so
    unrelated words cancel rather than pile up), counted with a damped
    weight, and the result scaled to unit length. Texts that share
    vocabulary therefore sit close together, which is enough for a payroll
    JD to rank payroll profiles first; it knows nothing about synonyms, so
    it is no substitute for a real model.
    """
    terms: list[str] = []
    for segment in _BREAK.split(text.lower()):
        words = [w for w in _WORD.findall(segment) if w not in _STOP_WORDS]
        terms += words
        terms += [f"{a} {b}" for a, b in pairwise(words)]
    # Text with no words in it still gets a (stable, non-zero) vector.
    terms = terms or [text]
    counts: dict[str, int] = {}
    for term in terms:
        counts[term] = counts.get(term, 0) + 1
    values = [0.0] * dim
    for term, count in counts.items():
        h = int.from_bytes(hashlib.blake2b(term.encode("utf-8"), digest_size=8).digest(), "big")
        sign = 1.0 if h & 1 else -1.0
        values[(h >> 1) % dim] += sign * (1.0 + math.log(count))
    norm = math.sqrt(sum(v * v for v in values))
    if norm == 0.0:
        # Every term cancelled (only possible at toy widths); pgvector cannot take the cosine of a zero vector.
        values[0], norm = 1.0, 1.0
    return [v / norm for v in values]
