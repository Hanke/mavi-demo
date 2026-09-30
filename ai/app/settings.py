from functools import lru_cache
from typing import Literal

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=".env", extra="ignore")

    # "fake" is the deterministic, key-free stand-in in app/fake.py.
    llm_provider: Literal["anthropic", "openai", "fake"] = "anthropic"
    anthropic_api_key: str = ""
    openai_api_key: str = ""
    # Empty means the provider's default in app/llm.py.
    llm_model: str = ""

    embedding_provider: Literal["openai", "local"] = "local"
    embedding_model: str = "text-embedding-3-small"
    # Must match the vector(N) width in infra/db/migrations.
    embedding_dim: int = 1536

    # Disk cache for LLM and embedding responses (app/cache.py): on reads and
    # writes, off does neither, refresh skips the read and overwrites the entry.
    ai_cache: Literal["on", "off", "refresh"] = "on"
    # Empty means ai/.cache next to this checkout (/app/.cache in the image).
    ai_cache_dir: str = ""

    database_url: str = ""

    # Shared canonical vocabulary (infra/taxonomy.json). Empty means the repo default
    # next to this checkout; the compose file points it at the mounted /app/infra copy.
    taxonomy_path: str = ""


@lru_cache
def get_settings() -> Settings:
    return Settings()
