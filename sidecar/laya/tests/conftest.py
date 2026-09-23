import asyncio
import os
import threading
from collections.abc import Mapping

import pytest
from httpx import ASGITransport, AsyncClient

os.environ.setdefault("LAYA_CLASSIFIER_TOKEN", "test-token")
from mindctl_laya.app import Settings, create_app


class FakeAgent:
    def __init__(self, error: Exception | None = None) -> None: self.error = error
    def predict(self, state: dict[str, object], questions: Mapping[str, object]) -> dict[str, object]:
        if self.error: raise self.error
        for name, question in questions.items():
            assert isinstance(question, dict), name
            if question["type"] == "score": assert isinstance(question["criteria"], list), name
        choice = {"type":"choice","choice":"T4","confidence":.9,"probabilities":{f"T{i}": 1. if i == 4 else 0. for i in range(7)},"action":{}}
        task = {"type":"choice","choice":"coding","confidence":1.,"probabilities":{"unknown":0.,"extraction":0.,"classification":0.,"generation":0.,"reasoning":0.,"coding":1.,"tool_use":0.,"multimodal":0.},"action":{}}
        def score(value: int, maximum: int) -> dict[str, object]:
            return {"type":"score","score":float(value),"confidence":.8,"probabilities":{str(i): 1. if i == value else 0. for i in range(maximum + 1)},"legend":{str(i):str(i) for i in range(maximum + 1)},"action":{}}
        return {"answers":{"minimum_tier":choice,"task_type":task,"reasoning_required":score(3,6),"coding_required":score(3,6),"blast_radius":score(1,4),"underspecified":{"type":"noul","noul":.1,"confidence":.9,"action":{}}}}


class BlockingSynchronousLoader:
    def __init__(self) -> None: self.started, self.release = threading.Event(), threading.Event()
    def __call__(self) -> FakeAgent:
        self.started.set()
        if not self.release.wait(1): raise RuntimeError("test loader timed out")
        return FakeAgent()


class Loader:
    def __init__(self, agent: FakeAgent) -> None:
        self.agent, self.started, self.allowed = agent, asyncio.Event(), asyncio.Event(); self.allowed.set()
    def block(self) -> None: self.allowed.clear()
    def release(self) -> None: self.allowed.set()
    async def __call__(self) -> FakeAgent:
        self.started.set(); await self.allowed.wait(); return self.agent


def valid_request() -> dict[str, object]:
    return {"schema_version":"mindctl.classifier.v1","state":{"prompt":"private prompt: implement a parser","features":{"needs_text":True,"input_tokens":120},"current_model":"current","available_models":["current","other"]}}
def auth() -> dict[str, str]: return {"authorization":"Bearer test-token"}
async def wait_until_ready(client: AsyncClient) -> None:
    for _ in range(50):
        if (await client.get("/readyz")).status_code == 200: return
        await asyncio.sleep(.01)
    raise AssertionError("sidecar never became ready")
@pytest.fixture
def anyio_backend() -> str: return "asyncio"
@pytest.fixture
async def client() -> AsyncClient:
    app = create_app(Settings(token="test-token"), Loader(FakeAgent()))
    async with app.router.lifespan_context(app):
        async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as test_client: yield test_client
