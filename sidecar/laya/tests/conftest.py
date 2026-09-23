import asyncio
import os

import pytest
from httpx import ASGITransport, AsyncClient

os.environ.setdefault("LAYA_CLASSIFIER_TOKEN", "test-token")

from mindctl_laya.app import Settings, create_app


class FakeAgent:
    def __init__(self, error: Exception | None = None) -> None:
        self.error = error

    def predict(self, state: dict[str, object], questions: dict[str, object]) -> dict[str, object]:
        if self.error is not None:
            raise self.error
        return {
            "answers": {
                "minimum_tier": {"choice": "T4", "confidence": 0.9, "probabilities": {"T0": 0.0, "T1": 0.0, "T2": 0.0, "T3": 0.1, "T4": 0.9, "T5": 0.0, "T6": 0.0}},
                "task_type": {"choice": "coding", "confidence": 1.0, "probabilities": {"unknown": 0.0, "extraction": 0.0, "classification": 0.0, "generation": 0.0, "reasoning": 0.0, "coding": 1.0, "tool_use": 0.0, "multimodal": 0.0}},
                "reasoning_required": {"score": 3.0, "confidence": 0.8, "probabilities": {str(i): 1.0 if i == 3 else 0.0 for i in range(7)}},
                "coding_required": {"score": 3.0, "confidence": 0.8, "probabilities": {str(i): 1.0 if i == 3 else 0.0 for i in range(7)}},
                "blast_radius": {"score": 1.0, "confidence": 0.8, "probabilities": {str(i): 1.0 if i == 1 else 0.0 for i in range(5)}},
                "underspecified": {"noul": 0.1},
            }
        }


class Loader:
    def __init__(self, agent: FakeAgent) -> None:
        self.agent = agent
        self.started = asyncio.Event()
        self.allowed = asyncio.Event()
        self.allowed.set()

    def block(self) -> None:
        self.allowed.clear()

    def release(self) -> None:
        self.allowed.set()

    async def __call__(self) -> FakeAgent:
        self.started.set()
        await self.allowed.wait()
        return self.agent


def valid_request() -> dict[str, object]:
    return {
        "schema_version": "mindctl.classifier.v1",
        "state": {
            "prompt": "private prompt: implement a parser",
            "features": {"needs_text": True, "input_tokens": 120},
            "current_model": "current",
            "available_models": ["current", "other"],
        },
    }


def auth() -> dict[str, str]:
    return {"authorization": "Bearer test-token"}


async def wait_until_ready(client: AsyncClient) -> None:
    for _ in range(50):
        if (await client.get("/readyz")).status_code == 200:
            return
        await asyncio.sleep(0.01)
    raise AssertionError("sidecar never became ready")


@pytest.fixture
def anyio_backend() -> str:
    return "asyncio"


@pytest.fixture
async def client() -> AsyncClient:
    app = create_app(Settings(token="test-token"), Loader(FakeAgent()))
    async with app.router.lifespan_context(app):
        async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as test_client:
            yield test_client
