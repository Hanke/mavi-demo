from fastapi import Depends, FastAPI
from pydantic import BaseModel, Field

from app import embeddings
from app.settings import Settings, get_settings

app = FastAPI(title="Mavi AI", version="0.1.0")


class HealthResponse(BaseModel):
    status: str
    llm_provider: str
    embedding_provider: str


class EmbedRequest(BaseModel):
    text: str = Field(min_length=1)


class EmbedResponse(BaseModel):
    embedding: list[float]
    dim: int
    provider: str


@app.get("/health", response_model=HealthResponse)
def health(settings: Settings = Depends(get_settings)) -> HealthResponse:
    return HealthResponse(
        status="ok",
        llm_provider=settings.llm_provider,
        embedding_provider=settings.embedding_provider,
    )


@app.post("/embed", response_model=EmbedResponse)
def embed(req: EmbedRequest, settings: Settings = Depends(get_settings)) -> EmbedResponse:
    vector = embeddings.embed(req.text, settings)
    return EmbedResponse(embedding=vector, dim=len(vector), provider=settings.embedding_provider)
