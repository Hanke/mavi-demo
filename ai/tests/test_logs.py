"""Request ids and structured logs: every request has an id (the caller's, when
it sends a usable one), the id is on every line logged for the request, and
each line is one JSON object."""

from __future__ import annotations

import json
import logging

import pytest
from fastapi.testclient import TestClient

from app import logs
from app.llm import ScriptedProvider
from app.main import app, get_provider
from app.settings import Settings, get_settings

app.dependency_overrides[get_settings] = lambda: Settings(embedding_provider="local", embedding_dim=8)
client = TestClient(app)


def test_the_callers_request_id_is_echoed():
    resp = client.get("/health", headers={logs.HEADER: "job-attempt-7"})
    assert resp.headers[logs.HEADER] == "job-attempt-7"


def test_a_request_without_an_id_is_given_one():
    first = client.get("/health").headers[logs.HEADER]
    second = client.get("/health").headers[logs.HEADER]
    assert len(first) == 16
    assert first != second


@pytest.mark.parametrize("given", ["two words", 'quo"te', "x" * 65, ""])
def test_an_unusable_id_is_replaced(given: str):
    got = client.get("/health", headers={logs.HEADER: given}).headers[logs.HEADER]
    assert got != given
    assert len(got) == 16


def test_an_error_response_names_its_request_too():
    resp = client.post("/embed", json={}, headers={logs.HEADER: "bad-request-1"})
    assert resp.status_code == 422
    assert resp.headers[logs.HEADER] == "bad-request-1"


def test_lines_logged_for_a_request_carry_its_id(caplog: pytest.LogCaptureFixture):
    """The access line, and a line the endpoint's own code logs from its worker thread."""
    provider = ScriptedProvider(["nonsense", "nonsense"])
    app.dependency_overrides[get_provider] = lambda: provider
    formatter = logs.JSONFormatter()
    lines: list[dict[str, object]] = []

    class Collect(logging.Handler):
        def emit(self, record: logging.LogRecord) -> None:
            # Formatted here, in the context the line was logged in, as the real handler does.
            lines.append(json.loads(formatter.format(record)))

    handler = Collect()
    logging.getLogger().addHandler(handler)
    try:
        with caplog.at_level("INFO"):
            resp = client.post("/parse-jd", json={"text": "Controller"}, headers={logs.HEADER: "trace-1"})
    finally:
        logging.getLogger().removeHandler(handler)
        app.dependency_overrides.pop(get_provider, None)
    assert resp.status_code == 502

    rejected = [line for line in lines if line["logger"] == "app.llm"]
    assert len(rejected) == 2
    assert all(line["request_id"] == "trace-1" and line["level"] == "WARN" for line in rejected)
    (access,) = [line for line in lines if line["msg"] == "request"]
    assert access["request_id"] == "trace-1"
    assert (access["method"], access["path"], access["status"]) == ("POST", "/parse-jd", 502)
    assert isinstance(access["duration_ms"], int)
    # What the request spent on the model, though it failed: two completions, neither from the cache.
    assert (access["llm_calls"], access["cache_hits"]) == (2, 0)
    assert access["service"] == "ai"
    assert access["level"] == "INFO"


def test_the_health_probe_is_logged_at_debug(caplog: pytest.LogCaptureFixture):
    with caplog.at_level("DEBUG", logger="app.logs"):
        client.get("/health")
    (record,) = [r for r in caplog.records if r.getMessage() == "request"]
    assert record.levelname == "DEBUG"


def test_a_line_outside_a_request_has_no_id_and_keeps_its_extras():
    record = logging.LogRecord("app.cache", logging.WARNING, __file__, 1, "could not store %s", ("k1",), None)
    record.key = "k1"
    line = json.loads(logs.JSONFormatter().format(record))
    assert line["msg"] == "could not store k1"
    assert line["key"] == "k1"
    assert line["logger"] == "app.cache"
    assert line["level"] == "WARN"
    assert "request_id" not in line


def test_configure_installs_one_json_handler():
    root = logging.getLogger()
    before = list(root.handlers)
    logs.configure()
    logs.configure()
    assert len([h for h in root.handlers if isinstance(h.formatter, logs.JSONFormatter)]) == 1
    assert [h for h in root.handlers if not isinstance(h.formatter, logs.JSONFormatter)] == [
        h for h in before if not isinstance(h.formatter, logs.JSONFormatter)
    ]
