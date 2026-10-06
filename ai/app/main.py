import json
from datetime import date
from functools import lru_cache
from typing import Any, Self

from fastapi import Depends, FastAPI, HTTPException, Request, Response
from fastapi.concurrency import run_in_threadpool
from fastapi.encoders import jsonable_encoder
from fastapi.exceptions import RequestValidationError
from pydantic import BaseModel, Field, field_validator, model_validator

from app import documents, embeddings, embedtext, extract, llm, logs, rubric, usage
from app.extract import RerankCandidate, RerankResult
from app.llm import InvalidOutputError, Provider
from app.schemas import CandidateProfile, Contact, Document, RoleRequirements
from app.settings import Settings, get_settings
from app.usage import LLMUsage

logs.configure()

# Route names double as operation ids so generated clients get `embed`, not `embed_embed_post`.
app = FastAPI(title="Mavi AI", version="0.1.0", generate_unique_id_function=lambda route: route.name)
# Every request gets an id (the caller's X-Request-ID, when it sends one), a usage meter and an access line.
app.add_middleware(logs.RequestContext)


@app.exception_handler(RequestValidationError)
async def invalid_request(_: Request, exc: RequestValidationError) -> Response:
    """FastAPI's 422, written as ASCII. The errors echo the rejected input, and
    a string with a lone surrogate (which is why it was rejected) has no UTF-8
    encoding: the default handler fails on it and the caller gets a 500."""
    body = json.dumps({"detail": jsonable_encoder(exc.errors())}, ensure_ascii=True)
    return Response(body, status_code=422, media_type="application/json")


MAX_TEXT_CHARS = 60_000  # ~15 pages; anything bigger is not a resume or a JD
MAX_RERANK_CANDIDATES = 50
MAX_EMBED_INPUTS = 256  # the seed embeds ~200 profiles in one request
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
    text: Document = Field(min_length=1, max_length=MAX_TEXT_CHARS)


class EmbedResponse(BaseModel):
    embedding: list[float]
    dim: int
    provider: str


class EmbedInput(BaseModel):
    """One thing to embed: exactly one of the three fields. A profile or a
    role's requirements is rendered to its canonical text (app/embedtext.py)
    first, so the two are embedded in a comparable form."""

    text: str | None = Field(default=None, min_length=1, max_length=MAX_TEXT_CHARS, description="Embedded as given.")
    profile: CandidateProfile | None = Field(default=None, description="Embedded as its canonical profile text.")
    requirements: RoleRequirements | None = Field(default=None, description="Embedded as its canonical role text.")

    @model_validator(mode="after")
    def _exactly_one(self) -> Self:
        given = [name for name in ("text", "profile", "requirements") if getattr(self, name) is not None]
        if len(given) != 1:
            raise ValueError(f"give exactly one of text, profile, requirements (got {', '.join(given) or 'none'})")
        return self

    def rendered(self) -> str:
        if self.profile is not None:
            return embedtext.profile_text(self.profile)
        if self.requirements is not None:
            return embedtext.role_text(self.requirements)
        return self.text or ""


class EmbedBatchRequest(BaseModel):
    inputs: list[EmbedInput] = Field(min_length=1, max_length=MAX_EMBED_INPUTS)


class EmbedBatchResponse(BaseModel):
    embeddings: list[list[float]] = Field(description="One vector per input, in the order given.")
    texts: list[str] = Field(description="The text each vector was computed from, in the same order.")
    dim: int
    provider: str


class ExtractTextResponse(BaseModel):
    text: str = Field(description="The text of the file, ready for /parse-resume or /parse-jd.")
    kind: documents.Kind = Field(description="What the file was found to be, from its content.")
    pages: int | None = Field(description="Pages of a PDF; null for a DOCX.")


class ParseResumeRequest(BaseModel):
    text: Document = Field(min_length=1, max_length=MAX_TEXT_CHARS, description="The resume as plain text.")
    as_of: date | None = Field(default=None, description="The date years_experience is counted to. Default: today.")


class ParseResumeResponse(BaseModel):
    contact: Contact
    profile: CandidateProfile
    provider: str


class ParseJDRequest(BaseModel):
    text: Document = Field(min_length=1, max_length=MAX_TEXT_CHARS, description="The job description as plain text.")


class ParseJDResponse(BaseModel):
    company: str | None
    requirements: RoleRequirements
    provider: str


class RerankRequest(BaseModel):
    role: Document = Field(
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
    rubric_version: str = Field(description="The version of the rubric (docs/rerank-rubric.md) the scores follow.")
    provider: str
    usage: LLMUsage = Field(description="What this request spent on the chat model.")


@app.get("/health", response_model=HealthResponse)
def health(settings: Settings = Depends(get_settings)) -> HealthResponse:
    return HealthResponse(
        status="ok",
        llm_provider=settings.llm_provider,
        embedding_provider=settings.embedding_provider,
    )


@app.post("/embed", response_model=EmbedResponse)
def embed(req: EmbedRequest, settings: Settings = Depends(get_settings)) -> EmbedResponse:
    try:
        vector = embeddings.embed(req.text, settings)
    except embeddings.EmbeddingError as e:
        raise HTTPException(status_code=502, detail=f"embedding provider error: {e}") from e
    return EmbedResponse(embedding=vector, dim=len(vector), provider=settings.embedding_provider)


@app.post("/embed-batch", response_model=EmbedBatchResponse)
def embed_batch(req: EmbedBatchRequest, settings: Settings = Depends(get_settings)) -> EmbedBatchResponse:
    texts = [item.rendered() for item in req.inputs]
    for i, text in enumerate(texts):
        if not text.strip():
            # Blank text, or a profile or a role with every field empty, which renders to nothing.
            raise HTTPException(status_code=422, detail=f"inputs[{i}] has nothing to embed")
    try:
        vectors = embeddings.embed_many(texts, settings)
    except embeddings.EmbeddingError as e:
        raise HTTPException(status_code=502, detail=f"embedding provider error: {e}") from e
    return EmbedBatchResponse(
        embeddings=vectors, texts=texts, dim=settings.embedding_dim, provider=settings.embedding_provider
    )


_UPLOAD_BODY: dict[str, Any] = {
    "requestBody": {
        "required": True,
        "description": "The file itself as the request body (not multipart): a PDF or a DOCX.",
        "content": {"application/octet-stream": {"schema": {"type": "string", "format": "binary"}}},
    }
}
_UPLOAD_ERRORS: dict[int | str, dict[str, Any]] = {
    413: {"description": f"The file is larger than {documents.megabytes(documents.MAX_UPLOAD_BYTES)}."},
    415: {"description": "The file is neither a PDF nor a DOCX."},
    422: {
        "description": f"The file is empty, damaged, password-protected or has no text, a PDF has more than "
        f"{documents.MAX_PDF_PAGES} pages, or the text is longer than {MAX_TEXT_CHARS} characters."
    },
}


@app.post("/extract-text", response_model=ExtractTextResponse, openapi_extra=_UPLOAD_BODY, responses=_UPLOAD_ERRORS)
async def extract_text(request: Request) -> ExtractTextResponse:
    """The text of an uploaded PDF or DOCX. The type is read from the file's
    content, not its name or Content-Type, and the limits are enforced here:
    the body is read no further than the size limit, whatever length it claims,
    and the file is opened in a separate process with a time limit."""
    declared = request.headers.get("content-length", "")
    if declared.isdigit() and int(declared) > documents.MAX_UPLOAD_BYTES:
        raise HTTPException(status_code=413, detail=str(documents.too_large()))
    data = bytearray()
    async for chunk in request.stream():
        data += chunk
        if len(data) > documents.MAX_UPLOAD_BYTES:
            raise HTTPException(status_code=413, detail=str(documents.too_large()))
    try:
        out = await run_in_threadpool(documents.extract_text_isolated, bytes(data), MAX_TEXT_CHARS)
    except documents.UploadError as e:
        raise HTTPException(status_code=e.status, detail=str(e)) from e
    return ExtractTextResponse(text=out.text, kind=out.kind, pages=out.pages)


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
def rerank(
    req: RerankRequest, provider: Provider = Depends(get_provider), settings: Settings = Depends(get_settings)
) -> RerankResponse:
    try:
        results = extract.rerank(provider, req.role, req.candidates)
    except llm.LLMError as e:
        raise _http_error(e) from e
    return RerankResponse(
        results=results,
        rubric_version=rubric.VERSION,
        provider=provider.name,
        usage=usage.report(provider.name, provider.model, settings),
    )


def _http_error(e: llm.LLMError) -> HTTPException:
    """Both failures are the upstream model's, so both are 502 (the Go client
    treats that as retryable), except a provider failure that the same input
    would only repeat (a refusal, an answer cut off at the output limit, a
    request the provider rejects as malformed or too large), which is a 422:
    the client does not retry a 4xx. The detail prefix says which, so the
    caller can tell a flaky provider from a prompt that needs work; app.llm
    logs the rejected output itself."""
    kind = "llm output invalid" if isinstance(e, InvalidOutputError) else "llm provider error"
    detail = f"{kind}: {e}"
    if len(detail) > MAX_ERROR_DETAIL_CHARS:
        detail = detail[: MAX_ERROR_DETAIL_CHARS - 3] + "..."
    permanent = isinstance(e, llm.ProviderError) and e.permanent
    return HTTPException(status_code=422 if permanent else 502, detail=detail)
