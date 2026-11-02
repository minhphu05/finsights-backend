from __future__ import annotations

import os
import sys
from contextlib import asynccontextmanager
from pathlib import Path
from typing import AsyncIterator

import grpc
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field

GENERATED = Path(__file__).resolve().parent / "generated"
if GENERATED.exists():
    sys.path.insert(0, str(GENERATED))

from finsights.v1 import ai_pb2, ai_pb2_grpc


class ChatRequest(BaseModel):
    user_id: str | None = None
    session_id: str | None = None
    message: str = Field(min_length=1, max_length=8000)


class ChatResponse(BaseModel):
    session_id: str | None = None
    answer: str
    stock_symbols: list[str] = Field(default_factory=list)
    article_slugs: list[str] = Field(default_factory=list)


class ProviderNotConfigured(Exception):
    pass


async def answer(_request: ChatRequest) -> ChatResponse:
    # The model/provider and grounding policy are intentionally left open.
    raise ProviderNotConfigured


class AIService(ai_pb2_grpc.AIServiceServicer):
    async def Ask(self, request: ai_pb2.AIAskRequest, context: grpc.aio.ServicerContext):
        if not request.message.strip():
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "message is required")
        await context.abort(
            grpc.StatusCode.UNIMPLEMENTED,
            "AI model provider and grounding policy have not been configured",
        )


@asynccontextmanager
async def lifespan(_app: FastAPI) -> AsyncIterator[None]:
    server = grpc.aio.server()
    ai_pb2_grpc.add_AIServiceServicer_to_server(AIService(), server)
    address = os.getenv("AI_GRPC_ADDR", "0.0.0.0:9094")
    server.add_insecure_port(address)
    await server.start()
    try:
        yield
    finally:
        await server.stop(grace=5)


app = FastAPI(title="Finsights AI Service", version="0.1.0", lifespan=lifespan)


@app.get("/healthz")
async def healthz() -> dict[str, str]:
    return {"status": "alive"}


@app.get("/readyz")
async def readyz() -> dict[str, str]:
    if not os.getenv("AI_PROVIDER_URL"):
        raise HTTPException(status_code=503, detail="AI provider is not configured")
    return {"status": "ready"}


@app.post("/v1/chat", response_model=ChatResponse)
async def chat(request: ChatRequest) -> ChatResponse:
    try:
        return await answer(request)
    except ProviderNotConfigured as exc:
        raise HTTPException(status_code=503, detail="AI provider is not configured") from exc
