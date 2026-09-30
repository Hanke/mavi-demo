from functools import lru_cache
from typing import Literal

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=".env", extra="ignore")

    llm_provider: Literal["anthropic", "openai"] = "anthropic"
    anthropic_api_key: str = ""
    openai_api_key: str = ""

    embedding_provider: Literal["openai", "local"] = "local"
    embedding_model: str = "text-embedding-3-small"
    # Must match the vector(N) width in infra/db/migrations.
    embedding_dim: int = 1536

    database_url: str = ""

    # Shared canonical vocabulary (infra/taxonomy.json). Empty means the repo default
    # next to this checkout; the compose file points it at the mounted /app/infra copy.
    taxonomy_path: str = ""


@lru_cache
def get_settings() -> Settings:
    return Settings()
