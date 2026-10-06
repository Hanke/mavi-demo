"""What a request spent on the chat model: calls, tokens and an estimated cost.

One `Meter` per HTTP request, started by the request middleware (app/logs.py)
and found through a context variable, so nothing between the endpoint and the
provider has to pass it along. `app.llm` counts every completion it asks for,
the cache says which of them it answered itself, and a provider reports the
tokens of a call that reached the model. Outside a request (the eval, the seed
generator, a unit test calling `extract` directly) there is no meter and all
three are no-ops.

The cost is an estimate: the tokens the provider reported, at the list price
of the configured model. It is what the request spent, so a completion
replayed from the cache costs nothing and counts as a cache hit, not a call.
"""

from __future__ import annotations

from contextvars import ContextVar
from dataclasses import dataclass

from pydantic import BaseModel, Field

from app.settings import Settings

# USD per million tokens (input, output): Anthropic's first-party list prices
# as of September 2026. A model that is not here has no known price, and its
# cost is reported as null unless LLM_INPUT_PRICE_PER_MTOK and LLM_OUTPUT_PRICE_PER_MTOK give one.
# The fake provider is free.
PRICES: dict[str, tuple[float, float]] = {
    "claude-fable-5-1": (10.00, 50.00),
    "claude-fable-5": (10.00, 50.00),
    "claude-opus-5-5": (4.00, 20.00),
    "claude-opus-5": (5.00, 25.00),
    "claude-opus-4-8": (5.00, 25.00),
    "claude-sonnet-5-5": (2.00, 10.00),
    "claude-sonnet-5": (2.00, 10.00),
    "claude-haiku-4-5": (1.00, 5.00),
}
FREE_PROVIDERS = frozenset({"fake"})


@dataclass
class Meter:
    """The running totals of one request. Only that request's thread writes to it."""

    completions: int = 0
    cache_hits: int = 0
    input_tokens: int = 0
    output_tokens: int = 0

    @property
    def llm_calls(self) -> int:
        """Completions that reached the provider."""
        return self.completions - self.cache_hits


_meter: ContextVar[Meter | None] = ContextVar("llm_usage_meter", default=None)


def start() -> Meter:
    """Begin metering the current context; the meter is returned to read at the end."""
    meter = Meter()
    _meter.set(meter)
    return meter


def current() -> Meter | None:
    return _meter.get()


def completion() -> None:
    """A completion was asked for, whoever answers it."""
    if (meter := _meter.get()) is not None:
        meter.completions += 1


def cache_hit() -> None:
    """The completion just asked for was replayed from the cache."""
    if (meter := _meter.get()) is not None:
        meter.cache_hits += 1


def tokens(input_tokens: int, output_tokens: int) -> None:
    """The tokens of a call that reached the model, as the provider counted them."""
    if (meter := _meter.get()) is not None:
        meter.input_tokens += input_tokens
        meter.output_tokens += output_tokens


class LLMUsage(BaseModel):
    """What a request spent on the chat model."""

    llm_calls: int = Field(description="Completions that reached the provider, a correction retry included.")
    cache_hits: int = Field(description="Completions replayed from the response cache instead: no call, no tokens.")
    input_tokens: int = Field(description="Prompt tokens of those calls, as the provider counted them.")
    output_tokens: int = Field(description="Output tokens of those calls, the model's thinking included.")
    estimated_cost_usd: float | None = Field(
        description="The tokens at the model's list price; null when no price is known for the model.",
        json_schema_extra={"format": "double"},
    )
    model: str = Field(description="The chat model the service is configured with.")


def price(provider: str, model: str, settings: Settings) -> tuple[float, float] | None:
    """USD per million (input, output) tokens for the model, or None when not known."""
    if provider in FREE_PROVIDERS:
        return (0.0, 0.0)
    if settings.llm_input_price_per_mtok is not None and settings.llm_output_price_per_mtok is not None:
        return (settings.llm_input_price_per_mtok, settings.llm_output_price_per_mtok)
    return PRICES.get(model)


def report(provider: str, model: str, settings: Settings, meter: Meter | None = None) -> LLMUsage:
    """The current request's totals (or `meter`'s) with the cost worked out."""
    meter = meter or _meter.get() or Meter()
    rates = price(provider, model, settings)
    cost = None
    if rates is not None:
        cost = round((meter.input_tokens * rates[0] + meter.output_tokens * rates[1]) / 1_000_000, 6)
    return LLMUsage(
        llm_calls=meter.llm_calls,
        cache_hits=meter.cache_hits,
        input_tokens=meter.input_tokens,
        output_tokens=meter.output_tokens,
        estimated_cost_usd=cost,
        model=model,
    )
