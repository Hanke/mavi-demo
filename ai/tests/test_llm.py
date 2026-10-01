"""The shared LLM client: schema-validated JSON with one correction retry."""

from __future__ import annotations

import pytest
from pydantic import BaseModel, ConfigDict, Field

from app import llm
from app.llm import InvalidOutputError, ProviderError, ScriptedProvider


class Answer(BaseModel):
    model_config = ConfigDict(extra="forbid")

    name: str
    count: int = Field(ge=0, le=10)
    note: str | None = None


def test_valid_output_is_returned_on_the_first_call():
    provider = ScriptedProvider(['{"name": "a", "count": 1, "note": null}'])
    out = llm.complete_json(provider, "sys", "user", Answer)
    assert out == Answer(name="a", count=1)
    assert len(provider.calls) == 1
    system, user, schema = provider.calls[0]
    assert (system, user) == ("sys", "user")
    assert schema["required"] == ["name", "count", "note"]
    assert schema["additionalProperties"] is False


def test_invalid_output_is_retried_with_the_errors_quoted_back():
    provider = ScriptedProvider(['{"name": "a", "count": 99}', '{"name": "a", "count": 3, "note": null}'])
    out = llm.complete_json(provider, "sys", "user", Answer)
    assert out.count == 3
    assert len(provider.calls) == 2
    retry_prompt = provider.calls[1][1]
    assert retry_prompt.startswith("user\n")
    assert "did not match the required schema" in retry_prompt
    assert "count" in retry_prompt
    assert '"count": 99' in retry_prompt


@pytest.mark.parametrize(
    "bad",
    [
        "not json at all",
        '{"name": "a", "count": -1, "note": null}',
        '{"name": "a", "count": 1, "note": null, "extra": true}',
        '{"count": 1}',
    ],
)
def test_output_that_never_validates_is_an_error_not_a_passthrough(bad: str):
    provider = ScriptedProvider([bad, bad])
    with pytest.raises(InvalidOutputError) as info:
        llm.complete_json(provider, "sys", "user", Answer)
    assert len(provider.calls) == llm.MAX_ATTEMPTS
    assert info.value.attempts == llm.MAX_ATTEMPTS
    assert info.value.model is Answer
    assert "Answer" in str(info.value)


def test_check_failures_count_as_invalid_output():
    provider = ScriptedProvider(['{"name": "x", "count": 1, "note": null}', '{"name": "y", "count": 1, "note": null}'])

    def must_be_y(a: Answer) -> str | None:
        return None if a.name == "y" else f"name must be y, got {a.name}"

    out = llm.complete_json(provider, "sys", "user", Answer, check=must_be_y)
    assert out.name == "y"
    assert "name must be y" in provider.calls[1][1]


def test_advice_is_retried_once_then_the_answer_is_returned_anyway():
    def prefers_y(a: Answer) -> str | None:
        return None if a.name == "y" else f"name should be y, got {a.name}"

    x, y = '{"name": "x", "count": 1, "note": null}', '{"name": "y", "count": 1, "note": null}'
    provider = ScriptedProvider([x, y])
    assert llm.complete_json(provider, "sys", "user", Answer, advise=prefers_y).name == "y"
    assert "name should be y" in provider.calls[1][1]

    provider = ScriptedProvider([x, x])
    assert llm.complete_json(provider, "sys", "user", Answer, advise=prefers_y).name == "x"
    assert len(provider.calls) == llm.MAX_ATTEMPTS

    # Advice does not excuse a failed check.
    provider = ScriptedProvider([x, x])
    with pytest.raises(InvalidOutputError):
        llm.complete_json(provider, "sys", "user", Answer, check=prefers_y, advise=lambda _: "anything")


def test_asking_again_on_advice_never_costs_the_usable_answer():
    """The retry came back broken: the first answer, which only drew advice, is still returned."""

    def prefers_y(a: Answer) -> str | None:
        return None if a.name == "y" else "name should be y"

    x = '{"name": "x", "count": 1, "note": null}'
    for broken in ("not json", '{"name": "z", "count": 99, "note": null}'):
        provider = ScriptedProvider([x, broken])
        assert llm.complete_json(provider, "sys", "user", Answer, advise=prefers_y).name == "x"
        assert len(provider.calls) == llm.MAX_ATTEMPTS


def test_provider_errors_are_not_retried():
    provider = ScriptedProvider([ProviderError("rate limited"), '{"name": "a", "count": 1, "note": null}'])
    with pytest.raises(ProviderError):
        llm.complete_json(provider, "sys", "user", Answer)
    assert len(provider.calls) == 1


def test_provider_schema_drops_constraints_the_model_cannot_decode():
    schema = llm.output_schema(Answer)
    assert "minimum" not in schema["properties"]["count"]
    assert "maximum" not in schema["properties"]["count"]
    assert "default" not in schema["properties"]["note"]
    # ...but the constraint is still enforced on the way back.
    provider = ScriptedProvider(['{"name": "a", "count": 11, "note": null}'] * 2)
    with pytest.raises(InvalidOutputError):
        llm.complete_json(provider, "sys", "user", Answer)


def test_build_provider_picks_the_configured_sdk():
    from app.settings import Settings

    assert llm.build_provider(Settings(llm_provider="anthropic", anthropic_api_key="k")).name == "anthropic"
    assert llm.build_provider(Settings(llm_provider="openai", openai_api_key="k")).name == "openai"


def test_missing_credentials_are_a_provider_error_not_a_crash(monkeypatch: pytest.MonkeyPatch):
    anthropic_provider = llm.AnthropicProvider(api_key="", model="m")

    def no_auth(**_: object) -> object:
        raise TypeError("Could not resolve authentication method")

    monkeypatch.setattr(anthropic_provider._client.messages, "create", no_auth)  # pyright: ignore[reportPrivateUsage]
    with pytest.raises(ProviderError, match="no credentials"):
        anthropic_provider.complete("s", "u", {})

    monkeypatch.delenv("OPENAI_API_KEY", raising=False)
    openai_provider = llm.OpenAIProvider(api_key="", model="m")  # constructing never raises
    with pytest.raises(ProviderError, match="no credentials"):
        openai_provider.complete("s", "u", {})
