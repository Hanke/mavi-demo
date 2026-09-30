"""The one place the service talks to a chat model.

Every call goes through `complete_json`: a system prompt, a user message and
a Pydantic model describing the answer. The provider is asked for JSON that
matches the model's schema, the text that comes back is validated with
Pydantic, and a response that fails validation is sent back to the model once
with the validation errors so it can correct itself. A second failure raises
`InvalidOutputError`; nothing that did not validate ever leaves this module.

`Provider` is the seam. `LLM_PROVIDER` picks the implementation: `anthropic`
or `openai` for a real model, `fake` for the deterministic key-free stand-in
in app/fake.py. Whichever it is, `build_provider` wraps it in a
`CachingProvider`, so a completion already on disk (app/cache.py) is replayed
instead of requested again. Unit tests script exact answers with a
`ScriptedProvider`, swapped in through `get_provider` dependency overrides.
"""

from __future__ import annotations

import logging
from collections.abc import Callable
from typing import TYPE_CHECKING, Any, Protocol

from pydantic import BaseModel, ValidationError

from app import cache as cachemod
from app.cache import Cache
from app.settings import Settings

if TYPE_CHECKING:
    import openai

log = logging.getLogger(__name__)

# One correction round: the first answer, then one retry with the errors attached.
MAX_ATTEMPTS = 2
MAX_OUTPUT_TOKENS = 16000
# How much of a rejected answer is quoted back to the model, and logged.
RETRY_QUOTE_CHARS = 4000
LOG_OUTPUT_CHARS = 2000

ANTHROPIC_MODEL = "claude-opus-5-5"
OPENAI_MODEL = "gpt-5"


class LLMError(Exception):
    """Base for failures the endpoints turn into HTTP errors."""


class ProviderError(LLMError):
    """The provider could not be reached or rejected the request (auth, rate limit, refusal)."""


class InvalidOutputError(LLMError):
    """The model's output failed schema validation on every attempt."""

    def __init__(self, model: type[BaseModel], attempts: int, last_error: str):
        self.model = model
        self.attempts = attempts
        self.last_error = last_error
        super().__init__(f"{model.__name__}: output failed validation after {attempts} attempts: {last_error}")


class Provider(Protocol):
    """A chat model that answers with JSON text for a schema."""

    name: str
    # Part of the cache key: the same prompt to a different model is a different answer.
    model: str

    def complete(self, system: str, user: str, schema: dict[str, Any]) -> str:
        """Return the raw text of one completion. Raises ProviderError."""
        ...


class AnthropicProvider:
    name = "anthropic"

    def __init__(self, api_key: str, model: str = ANTHROPIC_MODEL):
        import anthropic

        # Empty key: let the SDK fall back to the environment or an `ant auth login` profile.
        self._client = anthropic.Anthropic(api_key=api_key or None, timeout=120.0, max_retries=2)
        self.model = model
        self._errors = anthropic

    def complete(self, system: str, user: str, schema: dict[str, Any]) -> str:
        try:
            response = self._client.beta.messages.create(
                model=self.model,
                max_tokens=MAX_OUTPUT_TOKENS,
                system=system,
                messages=[{"role": "user", "content": user}],
                output_config={"effort": "medium", "format": {"type": "json_schema", "schema": schema}},
                # A safety-classifier decline is re-run on a fallback model inside
                # the same call instead of failing the request (as in app/seedgen).
                betas=["server-side-fallback-2026-07-01"],
                fallbacks="default",
            )
        except self._errors.RateLimitError as e:
            raise ProviderError(f"anthropic: rate limited: {e.message}") from e
        except self._errors.APIStatusError as e:
            raise ProviderError(f"anthropic: {e.status_code}: {e.message}") from e
        except self._errors.APIConnectionError as e:
            raise ProviderError(f"anthropic: connection failed: {e}") from e
        except TypeError as e:
            # The SDK resolves credentials per request and raises TypeError when
            # it finds none (no key, no `ant auth login` profile).
            raise ProviderError(f"anthropic: no credentials configured: {e}") from e
        if response.stop_reason == "refusal":
            raise ProviderError(f"anthropic: request refused ({response.stop_details})")
        if response.stop_reason == "max_tokens":
            raise ProviderError("anthropic: output truncated at max_tokens")
        text = next((b.text for b in response.content if b.type == "text"), None)
        if text is None:
            raise ProviderError("anthropic: response carried no text block")
        return text


class OpenAIProvider:
    name = "openai"

    def __init__(self, api_key: str, model: str = OPENAI_MODEL):
        import openai

        self._api_key = api_key
        self.model = model
        self._errors = openai
        self._client: openai.OpenAI | None = None

    def _connect(self) -> openai.OpenAI:
        # Built on first use: the constructor raises when no key is configured,
        # and that belongs to the request that needed it, not to app start-up.
        if self._client is None:
            try:
                self._client = self._errors.OpenAI(api_key=self._api_key or None, timeout=120.0, max_retries=2)
            except self._errors.OpenAIError as e:
                raise ProviderError(f"openai: no credentials configured: {e}") from e
        return self._client

    def complete(self, system: str, user: str, schema: dict[str, Any]) -> str:
        try:
            response = self._connect().chat.completions.create(
                model=self.model,
                messages=[{"role": "system", "content": system}, {"role": "user", "content": user}],
                response_format={
                    "type": "json_schema",
                    "json_schema": {"name": "output", "schema": schema, "strict": False},
                },
            )
        except self._errors.RateLimitError as e:
            raise ProviderError(f"openai: rate limited: {e}") from e
        except self._errors.APIStatusError as e:
            raise ProviderError(f"openai: {e.status_code}: {e}") from e
        except self._errors.APIConnectionError as e:
            raise ProviderError(f"openai: connection failed: {e}") from e
        choice = response.choices[0]
        if choice.message.refusal:
            raise ProviderError(f"openai: request refused ({choice.message.refusal})")
        if choice.finish_reason == "length":
            raise ProviderError("openai: output truncated")
        if choice.message.content is None:
            raise ProviderError("openai: response carried no content")
        return choice.message.content


class ScriptedProvider:
    """Test double: replays canned responses in order, recording every prompt it was given."""

    name = "scripted"
    model = "scripted"

    def __init__(self, responses: list[str | Exception]):
        self.responses = list(responses)
        self.calls: list[tuple[str, str, dict[str, Any]]] = []

    def queue(self, *responses: str | Exception) -> None:
        self.responses.extend(responses)

    def complete(self, system: str, user: str, schema: dict[str, Any]) -> str:
        self.calls.append((system, user, schema))
        if not self.responses:
            raise AssertionError("ScriptedProvider ran out of responses")
        nxt = self.responses.pop(0)
        if isinstance(nxt, Exception):
            raise nxt
        return nxt


class CachingProvider:
    """A provider with the disk cache in front of it.

    The key is the provider, its model, both prompts and the schema, so the
    correction retry (a different user prompt) is its own entry and a replayed
    run takes the same path as the original. A ProviderError is not stored."""

    def __init__(self, inner: Provider, cache: Cache):
        self.inner = inner
        self.cache = cache
        self.name = inner.name
        self.model = inner.model

    def complete(self, system: str, user: str, schema: dict[str, Any]) -> str:
        key = cachemod.key("llm", provider=self.name, model=self.model, system=system, user=user, schema=schema)
        return self.cache.get_or_call(
            key, lambda: self.inner.complete(system, user, schema), kind="llm", provider=self.name, model=self.model
        )


def build_provider(settings: Settings) -> CachingProvider:
    """The provider the settings select, behind the cache in the mode the
    settings select. Constructing one touches neither the network nor the disk."""
    inner: Provider
    if settings.llm_provider == "fake":
        from app.fake import FakeProvider

        inner = FakeProvider()
    elif settings.llm_provider == "openai":
        inner = OpenAIProvider(settings.openai_api_key, settings.llm_model or OPENAI_MODEL)
    else:
        inner = AnthropicProvider(settings.anthropic_api_key, settings.llm_model or ANTHROPIC_MODEL)
    return CachingProvider(inner, cachemod.from_settings(settings))


# Keywords the providers' constrained-decoding modes reject or ignore. They
# are dropped from the schema sent to the model and enforced by Pydantic on the
# way back, so a violation still surfaces as a validation error and a retry.
_UNSUPPORTED_KEYWORDS = frozenset(
    {"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "minLength", "maxLength", "pattern", "default"}
)


def output_schema(model: type[BaseModel]) -> dict[str, Any]:
    """The JSON schema handed to the provider: the model's own, with every
    property required and no extras, which is what strict JSON modes expect.
    Nullable fields stay nullable so `required` does not force a value."""
    schema = model.model_json_schema(mode="validation")
    _provider_safe(schema)
    return schema


def _provider_safe(node: Any) -> None:
    if isinstance(node, list):
        for item in node:  # pyright: ignore[reportUnknownVariableType]
            _provider_safe(item)
        return
    if not isinstance(node, dict):
        return
    d: dict[str, Any] = node  # pyright: ignore[reportUnknownVariableType]
    if d.get("type") == "object" and "properties" in d:
        d["required"] = list(d["properties"])
        d["additionalProperties"] = False
    for key in _UNSUPPORTED_KEYWORDS:
        d.pop(key, None)
    for value in d.values():
        _provider_safe(value)


def complete_json[T: BaseModel](
    provider: Provider,
    system: str,
    user: str,
    model: type[T],
    *,
    check: Callable[[T], str | None] | None = None,
    max_attempts: int = MAX_ATTEMPTS,
) -> T:
    """Ask the provider for a `model` and return the validated instance.

    A response that is not JSON, or is JSON that `model` rejects, is retried
    with the errors quoted back in the user message. `check` runs on a parsed
    instance and returns a problem description (or None) for rules the schema
    cannot express, e.g. "every candidate id must appear exactly once"; a
    problem counts as a failed attempt too. After `max_attempts` the last
    failure is raised as InvalidOutputError."""
    schema = output_schema(model)
    prompt = user
    last_error = ""
    for attempt in range(1, max_attempts + 1):
        text = provider.complete(system, prompt, schema)
        try:
            parsed = model.model_validate_json(text)
        except ValidationError as e:
            last_error = _describe(e)
        else:
            problem = check(parsed) if check else None
            if problem is None:
                return parsed
            last_error = f"- {problem}"
        log.warning(
            "%s: %s output failed validation (attempt %d/%d):\n%s\n--- output ---\n%s",
            provider.name,
            model.__name__,
            attempt,
            max_attempts,
            last_error,
            text[:LOG_OUTPUT_CHARS],
        )
        prompt = (
            f"{user}\n\n"
            f"Your previous answer did not match the required schema. Problems:\n{last_error}\n\n"
            f"Previous answer:\n{text[:RETRY_QUOTE_CHARS]}\n\n"
            "Return a corrected JSON object that satisfies the schema."
        )
    raise InvalidOutputError(model, max_attempts, last_error)


def _describe(e: ValidationError) -> str:
    lines: list[str] = []
    for err in e.errors(include_url=False):
        loc = ".".join(str(p) for p in err["loc"]) or "<root>"
        lines.append(f"- {loc}: {err['msg']}")
    return "\n".join(lines) or str(e)
