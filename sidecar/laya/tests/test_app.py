import pytest
from httpx import ASGITransport, AsyncClient

from mindctl_laya.app import Settings, create_app

from .conftest import FakeAgent, Loader, auth, valid_request, wait_until_ready


@pytest.mark.anyio
async def test_ready_is_unavailable_until_model_load_completes():
    loader = Loader(FakeAgent())
    loader.block()
    app = create_app(Settings(token="test-token"), loader)
    async with app.router.lifespan_context(app):
        async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
            await loader.started.wait()
            assert (await client.get("/healthz")).status_code == 200
            assert (await client.get("/readyz")).status_code == 503
            loader.release()
            await wait_until_ready(client)
            assert (await client.get("/readyz")).status_code == 200


@pytest.mark.anyio
async def test_inference_error_is_redacted():
    app = create_app(Settings(token="test-token"), Loader(FakeAgent(RuntimeError("private prompt exploded"))))
    async with app.router.lifespan_context(app):
        async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
            await wait_until_ready(client)
            response = await client.post("/v1/classify", headers=auth(), json=valid_request())
    assert response.status_code == 503
    assert "private prompt" not in response.text
    assert "exploded" not in response.text
