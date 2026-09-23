import json

import pytest

from mindctl_laya.questions import QUESTIONS

from .conftest import auth, valid_request


@pytest.mark.anyio
async def test_classify_requires_token_and_emits_all_six_answers(client):
    unauthorized = await client.post("/v1/classify", json=valid_request())
    assert unauthorized.status_code == 401

    response = await client.post("/v1/classify", headers=auth(), json=valid_request())
    assert response.status_code == 200
    body = response.json()
    assert body["schema_version"] == "mindctl.classifier.v1"
    assert body["classifier"] == {
        "name": "laya",
        "repository": "convaiinnovations/laya",
        "revision": "5e7b2b1b8ca2ecdd3f2322d94069c9b6ce7e844b",
        "variant": "typed-decisions",
    }
    assert set(body["answers"]) == {
        "minimum_tier",
        "task_type",
        "reasoning_required",
        "coding_required",
        "blast_radius",
        "underspecified",
    }


@pytest.mark.anyio
async def test_rejects_unknown_input_and_oversized_body(client):
    unknown = valid_request()
    unknown["unexpected"] = True
    response = await client.post("/v1/classify", headers=auth(), json=unknown)
    assert response.status_code == 422
    assert "private prompt" not in response.text

    unsupported_schema = valid_request()
    unsupported_schema["schema_version"] = "private prompt unsupported schema"
    response = await client.post("/v1/classify", headers=auth(), json=unsupported_schema)
    assert response.status_code == 422
    assert "private prompt" not in response.text

    body = valid_request()
    body["state"]["prompt"] = "x" * ((1 << 20) + 1)
    response = await client.post("/v1/classify", headers=auth(), content=json.dumps(body))
    assert response.status_code == 413


def test_questions_are_exactly_the_fixed_six_requirement_signals():
    assert set(QUESTIONS) == {
        "minimum_tier",
        "task_type",
        "reasoning_required",
        "coding_required",
        "blast_radius",
        "underspecified",
    }
    assert QUESTIONS["minimum_tier"]["type"] == "choice"
    assert set(QUESTIONS["minimum_tier"]["criteria"]) == {f"T{i}" for i in range(7)}
    assert QUESTIONS["task_type"]["type"] == "choice"
    assert len(QUESTIONS["task_type"]["criteria"]) == 8
    assert [QUESTIONS[name]["type"] for name in ("reasoning_required", "coding_required", "blast_radius")] == ["score", "score", "score"]
    assert QUESTIONS["underspecified"]["type"] == "noul"
