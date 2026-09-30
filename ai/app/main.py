from datetime import date
from functools import lru_cache

from fastapi import Depends, FastAPI, HTTPException
from pydantic import BaseModel, Field, field_validator

from app import embeddings, extract, llm
from app.extract import RerankCandidate, RerankResult
from app.llm import InvalidOutputError, Provider
from app.schemas import CandidateProfile, Contact, RoleRequirements
from app.settings import Settings, get_settings

# Route names double as operation ids so generated clients get `embed`, not `embed_embed_post`.
app = FastAPI(title="Mavi AI", version="0.1.0", generate_unique_id_function=lambda route: route.name)

MAX_TEXT_CHARS = 60_000  # ~15 pages; anything bigger is not a resume or a JD
MAX_RERANK_CANDIDATES = 50
MAX_ERROR_DETAIL_CHARS = 2000  # the Go client reads at most 4KB of an error body


@lru_cache
def _provider(
    *, llm_provider: str, llm_model: str, anthropic_api_key: str, openai_api_key: str, ai_cache: str, ai_cache_dir: str
) -> Provider:
    return llm.build_provider(
        Settings(
            llm_provider=llm_provider,  # pyright: ignore[reportArgumentType]
            llm_model=llm_model,
            anthropic_api_key=anthropic_api_key,
            openai_api_key=openai_api_key,
            ai_cache=ai_cache,  # pyright: ignore[reportArgumentType]
            ai_cache_dir=ai_cache_dir,
        )
    )


def get_provider(settings: Settings = Depends(get_settings)) -> Provider:
    """One SDK client per settings combination, shared across requests. Tests override this with a ScriptedProvider."""
    return _provider(
        llm_provider=settings.llm_provider,
        llm_model=settings.llm_model,
        anthropic_api_key=settings.anthropic_api_key,
        openai_api_key=settings.openai_api_key,
        ai_cache=settings.ai_cache,
        ai_cache_dir=settings.ai_cache_dir,
    )


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


class ParseResumeRequest(BaseModel):
    text: str = Field(min_length=1, max_length=MAX_TEXT_CHARS, description="The resume as plain text.")
    as_of: date | None = Field(default=None, description="The date years_experience is counted to. Default: today.")


class ParseResumeResponse(BaseModel):
    contact: Contact
    profile: CandidateProfile
    provider: str


class ParseJDRequest(BaseModel):
    text: str = Field(min_length=1, max_length=MAX_TEXT_CHARS, description="The job description as plain text.")


class ParseJDResponse(BaseModel):
    company: str | None
    requirements: RoleRequirements
    provider: str


class RerankRequest(BaseModel):
    role: str = Field(
        min_length=1, max_length=MAX_TEXT_CHARS, description="The job description, or a rendering of the role."
    )
    candidates: list[RerankCandidate] = Field(min_length=1, max_length=MAX_RERANK_CANDIDATES)

    @field_validator("candidates")
    @classmethod
    def _unique_ids(cls, candidates: list[RerankCandidate]) -> list[RerankCandidate]:
        ids = [c.id for c in candidates]
        if len(set(ids)) != len(ids):
            raise ValueError("candidate ids must be unique")
        return candidates


class RerankResponse(BaseModel):
    results: list[RerankResult] = Field(description="Every candidate, best first.")
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


@app.post("/parse-resume", response_model=ParseResumeResponse)
def parse_resume(req: ParseResumeRequest, provider: Provider = Depends(get_provider)) -> ParseResumeResponse:
    try:
        out = extract.parse_resume(provider, req.text, req.as_of)
    except llm.LLMError as e:
        raise _http_error(e) from e
    return ParseResumeResponse(contact=out.contact, profile=out.profile, provider=provider.name)


@app.post("/parse-jd", response_model=ParseJDResponse)
def parse_jd(req: ParseJDRequest, provider: Provider = Depends(get_provider)) -> ParseJDResponse:
    try:
        out = extract.parse_jd(provider, req.text)
    except llm.LLMError as e:
        raise _http_error(e) from e
    return ParseJDResponse(company=out.company, requirements=out.requirements, provider=provider.name)


@app.post("/rerank", response_model=RerankResponse)
def rerank(req: RerankRequest, provider: Provider = Depends(get_provider)) -> RerankResponse:
    try:
        results = extract.rerank(provider, req.role, req.candidates)
    except llm.LLMError as e:
        raise _http_error(e) from e
    return RerankResponse(results=results, provider=provider.name)


def _http_error(e: llm.LLMError) -> HTTPException:
    """Both failures are the upstream model's, so both are 502 (the Go client
    treats that as retryable). The detail prefix says which, so the caller can
    tell a flaky provider from a prompt that needs work; app.llm logs the
    rejected output itself."""
    kind = "llm output invalid" if isinstance(e, InvalidOutputError) else "llm provider error"
    detail = f"{kind}: {e}"
    if len(detail) > MAX_ERROR_DETAIL_CHARS:
        detail = detail[: MAX_ERROR_DETAIL_CHARS - 3] + "..."
    return HTTPException(status_code=502, detail=detail)
