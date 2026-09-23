import os
import secrets
from contextlib import asynccontextmanager
from dataclasses import dataclass
from typing import Any

from fastapi import Depends, FastAPI, HTTPException, Request, status
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse

from .contract import (
    MAX_BODY_BYTES,
    ClassifierMetadata,
    ClassifyRequest,
    ClassifyResponse,
    ChoiceAnswer,
    NoulAnswer,
    ScoreAnswer,
)
from .questions import QUESTIONS
from .service import AgentLoader, LayaService, default_agent_loader


@dataclass(frozen=True)
class Settings:
    token: str
    model_revision: str = "5e7b2b1b8ca2ecdd3f2322d94069c9b6ce7e844b"
    model_variant: str = "typed-decisions"

    @classmethod
    def from_environment(cls) -> "Settings":
        token = os.environ.get("LAYA_CLASSIFIER_TOKEN", "")
        if not token:
            raise RuntimeError("LAYA_CLASSIFIER_TOKEN is required")
        return cls(
            token=token,
            model_revision=os.environ.get("LAYA_MODEL_REVISION", cls.model_revision),
            model_variant=os.environ.get("LAYA_MODEL_VARIANT", cls.model_variant),
        )

    def metadata(self) -> ClassifierMetadata:
        return ClassifierMetadata(revision=self.model_revision, variant=self.model_variant)


class BodyTooLarge(Exception):
    pass


class BodyLimitMiddleware:
    def __init__(self, app: Any, maximum: int = MAX_BODY_BYTES) -> None:
        self.app = app
        self.maximum = maximum

    async def __call__(self, scope: dict[str, Any], receive: Any, send: Any) -> None:
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return
        for key, value in scope.get("headers", []):
            if key.lower() == b"content-length" and int(value) > self.maximum:
                await JSONResponse({"detail": "request body too large"}, status_code=413)(scope, receive, send)
                return
        seen = 0

        async def limited_receive() -> dict[str, Any]:
            nonlocal seen
            message = await receive()
            if message["type"] == "http.request":
                seen += len(message.get("body", b""))
                if seen > self.maximum:
                    raise BodyTooLarge
            return message

        try:
            await self.app(scope, limited_receive, send)
        except BodyTooLarge:
            await JSONResponse({"detail": "request body too large"}, status_code=413)(scope, receive, send)


def _require_token(settings: Settings, request: Request) -> None:
    authorization = request.headers.get("authorization", "")
    scheme, _, supplied = authorization.partition(" ")
    if scheme.lower() != "bearer" or not supplied or not secrets.compare_digest(supplied, settings.token):
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, "unauthorized", headers={"WWW-Authenticate": "Bearer"})


def _answers(raw: dict[str, Any]) -> dict[str, ChoiceAnswer | ScoreAnswer | NoulAnswer]:
    source = raw.get("answers")
    if not isinstance(source, dict) or set(source) != set(QUESTIONS):
        raise ValueError("invalid Laya answers")
    converted: dict[str, ChoiceAnswer | ScoreAnswer | NoulAnswer] = {}
    for name, question in QUESTIONS.items():
        answer = source[name]
        if not isinstance(answer, dict):
            raise ValueError("invalid Laya answer")
        if question["type"] == "choice":
            converted[name] = ChoiceAnswer(choice=answer["choice"], confidence=answer["confidence"], probabilities=answer["probabilities"])
        elif question["type"] == "score":
            converted[name] = ScoreAnswer(score=answer["score"], confidence=answer["confidence"], probabilities=answer["probabilities"], legend=answer["legend"])
        else:
            converted[name] = NoulAnswer(noul=answer["noul"])
    return converted


def create_app(settings: Settings | None = None, load_agent: AgentLoader | None = None) -> FastAPI:
    settings = settings or Settings.from_environment()
    loader = load_agent or (lambda: default_agent_loader(settings.model_revision, settings.model_variant))
    service = LayaService(loader)

    @asynccontextmanager
    async def lifespan(_: FastAPI):
        service.start()
        yield
        await service.close()

    app = FastAPI(title="Mindctl Laya classifier", version="1", lifespan=lifespan)
    app.add_middleware(BodyLimitMiddleware)

    @app.exception_handler(RequestValidationError)
    async def invalid_request(_: Request, __: RequestValidationError) -> JSONResponse:
        return JSONResponse({"detail": "invalid request"}, status_code=422)

    @app.get("/healthz")
    async def healthz() -> dict[str, str]:
        return {"status": "ok"}

    @app.get("/readyz")
    async def readyz() -> dict[str, str]:
        if not service.ready:
            raise HTTPException(status.HTTP_503_SERVICE_UNAVAILABLE, "classifier unavailable")
        return {"status": "ready"}

    @app.post("/v1/classify", response_model=ClassifyResponse)
    async def classify(body: ClassifyRequest, request: Request) -> ClassifyResponse:
        _require_token(settings, request)
        try:
            result = await service.predict(body.state.model_dump(mode="json"))
            return ClassifyResponse(classifier=settings.metadata(), answers=_answers(result))
        except HTTPException:
            raise
        except Exception:
            raise HTTPException(status.HTTP_503_SERVICE_UNAVAILABLE, "classifier unavailable") from None

    return app


app = create_app()
