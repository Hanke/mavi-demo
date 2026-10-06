from functools import lru_cache
from typing import Literal

from pydantic import field_validator
from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=".env", extra="ignore")

    # "fake" is the deterministic, key-free stand-in in app/fake.py.
    llm_provider: Literal["anthropic", "openai", "fake"] = "anthropic"
    anthropic_api_key: str = ""
    openai_api_key: str = ""
    # Empty means the provider's default in app/llm.py.
    llm_model: str = ""
    # USD per million input and output tokens, for the cost estimate of a
    # request (app/usage.py). Both empty means the list price of the model,
    # when app/usage.py knows one.
    llm_input_price_per_mtok: float | None = None
    llm_output_price_per_mtok: float | None = None

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

    @field_validator("llm_input_price_per_mtok", "llm_output_price_per_mtok", mode="before")
    @classmethod
    def _blank_is_unset(cls, value: object) -> object:
        # Compose passes a variable that is not set as an empty string.
        return None if isinstance(value, str) and not value.strip() else value


@lru_cache
def get_settings() -> Settings:
    return Settings()
