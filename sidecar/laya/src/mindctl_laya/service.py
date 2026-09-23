import asyncio
import inspect
from collections.abc import Awaitable, Callable, Mapping
from typing import Any, Protocol

from fastapi.concurrency import run_in_threadpool
from huggingface_hub import snapshot_download

from .questions import native_questions


class Agent(Protocol):
    def predict(self, state: dict[str, Any], questions: Mapping[str, Mapping[str, Any]]) -> dict[str, Any]: ...


AgentLoader = Callable[[], Agent | Awaitable[Agent]]


class LayaService:
    def __init__(self, load_agent: AgentLoader) -> None:
        self._load_agent = load_agent
        self._agent: Agent | None = None
        self._task: asyncio.Task[None] | None = None
        self._load_failed = False
        self._lock = asyncio.Lock()

    def start(self) -> None:
        self._task = asyncio.create_task(self._load())

    async def _load(self) -> None:
        try:
            loaded = await asyncio.to_thread(self._load_agent)
            self._agent = await loaded if inspect.isawaitable(loaded) else loaded
        except Exception:
            self._load_failed = True

    async def close(self) -> None:
        if self._task is not None and not self._task.done():
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass

    @property
    def ready(self) -> bool:
        return self._agent is not None and not self._load_failed

    async def predict(self, state: dict[str, Any]) -> dict[str, Any]:
        if self._agent is None:
            raise RuntimeError("classifier unavailable")
        async with self._lock:
            return await run_in_threadpool(self._agent.predict, state, native_questions())


def default_agent_loader(revision: str, variant: str) -> Agent:
    local_snapshot = snapshot_download("convaiinnovations/laya", revision=revision)
    import laya

    return laya.load(local_snapshot, subfolder=variant)
