import asyncio

import pytest
from httpx import ASGITransport, AsyncClient

from mindctl_laya.app import Settings, create_app
from .conftest import BlockingSynchronousLoader, FakeAgent, Loader, auth, valid_request, wait_until_ready


@pytest.mark.anyio
async def test_ready_is_unavailable_until_model_load_completes():
    loader = Loader(FakeAgent()); loader.block(); app = create_app(Settings(token="test-token"), loader)
    async with app.router.lifespan_context(app):
        async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
            await loader.started.wait()
            assert (await client.get("/healthz")).status_code == 200
            assert (await client.get("/readyz")).status_code == 503
            loader.release(); await wait_until_ready(client)


@pytest.mark.anyio
async def test_inference_error_is_redacted():
    app = create_app(Settings(token="test-token"), Loader(FakeAgent(RuntimeError("private prompt exploded"))))
    async with app.router.lifespan_context(app):
        async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
            await wait_until_ready(client); response = await client.post("/v1/classify", headers=auth(), json=valid_request())
    assert response.status_code == 503 and "private prompt" not in response.text


@pytest.mark.anyio
async def test_synchronous_loading_does_not_block_health_checks():
    loader = BlockingSynchronousLoader(); app = create_app(Settings(token="test-token"), loader)
    async with app.router.lifespan_context(app):
        async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
            await asyncio.to_thread(loader.started.wait, .2)
            assert (await client.get("/healthz")).status_code == 200
            assert (await client.get("/readyz")).status_code == 503
            loader.release.set(); await wait_until_ready(client)


@pytest.mark.anyio
async def test_model_load_failure_is_logged(caplog):
    def failing_loader() -> FakeAgent: raise RuntimeError("snapshot_download: permission denied")
    app = create_app(Settings(token="test-token"), failing_loader)
    with caplog.at_level("ERROR", logger="mindctl_laya.service"):
        async with app.router.lifespan_context(app):
            async with AsyncClient(transport=ASGITransport(app=app), base_url="http://test") as client:
                for _ in range(50):
                    if any("model load failed" in r.message for r in caplog.records): break
                    await asyncio.sleep(.01)
                assert (await client.get("/readyz")).status_code == 503
    record = next(r for r in caplog.records if "model load failed" in r.message)
    assert record.exc_info and "permission denied" in caplog.text
