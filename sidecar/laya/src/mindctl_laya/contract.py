from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator


SCHEMA_VERSION = "mindctl.classifier.v1"
MAX_BODY_BYTES = 1 << 20


class StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class State(StrictModel):
    prompt: str
    features: dict[str, Any]
    current_model: str
    available_models: list[str]


class ClassifyRequest(StrictModel):
    schema_version: str
    state: State

    @model_validator(mode="after")
    def require_supported_schema(self) -> "ClassifyRequest":
        if self.schema_version != SCHEMA_VERSION:
            raise ValueError("unsupported schema version")
        return self


class ClassifierMetadata(StrictModel):
    name: Literal["laya"] = "laya"
    repository: Literal["convaiinnovations/laya"] = "convaiinnovations/laya"
    revision: str
    variant: Literal["typed-decisions"] = "typed-decisions"


class ChoiceAnswer(StrictModel):
    type: Literal["choice"] = "choice"
    choice: str
    confidence: float = Field(ge=0, le=1)
    probabilities: dict[str, float]


class ScoreAnswer(StrictModel):
    type: Literal["score"] = "score"
    score: float
    confidence: float = Field(ge=0, le=1)
    probabilities: dict[str, float]
    legend: dict[str, str]


class NoulAnswer(StrictModel):
    type: Literal["noul"] = "noul"
    noul: float = Field(ge=0, le=1)


class ClassifyResponse(StrictModel):
    schema_version: Literal["mindctl.classifier.v1"] = SCHEMA_VERSION
    classifier: ClassifierMetadata
    answers: dict[str, ChoiceAnswer | ScoreAnswer | NoulAnswer]
