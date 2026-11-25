from __future__ import annotations

import asyncio
import os
import sys
from contextlib import asynccontextmanager
from pathlib import Path
from typing import AsyncIterator

import grpc
import jwt
from fastapi import FastAPI, Header, HTTPException, Query
from pydantic import BaseModel, ConfigDict, Field, field_validator

GENERATED = Path(__file__).resolve().parent / "generated"
if GENERATED.exists():
    sys.path.insert(0, str(GENERATED))

from finsights.v1 import ai_pb2, ai_pb2_grpc

from .agent.admission import AdmissionController, RateLimitExceeded, ServiceBusy
from .agent.harness import Harness
from .agent.tools import ToolContext, ToolRegistry
from .config import Settings
from .database import ChatHistoryStore
from .llm import OpenAICompatibleClient
from .rag.service import RagService


class ChatRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    session_id: str | None = Field(default=None, max_length=36)
    message: str = Field(min_length=1, max_length=8000)

    @field_validator("message", mode="before")
    @classmethod
    def trim_message(cls, value: object) -> object:
        return value.strip() if isinstance(value, str) else value


class ChatResponse(BaseModel):
    session_id: str
    answer: str
    stock_symbols: list[str] = Field(default_factory=list)
    article_slugs: list[str] = Field(default_factory=list)
    references: list[dict[str, str]] = Field(default_factory=list)


class ChatRuntime:
    def __init__(self, settings: Settings, history: ChatHistoryStore, harness: Harness | None,
                 market_client, user_client, rag: RagService | None):
        self.settings = settings
        self.history = history
        self.harness = harness
        self.market_client = market_client
        self.user_client = user_client
        self.rag = rag
        self.admission = AdmissionController(settings.max_requests_per_minute, settings.max_concurrent_turns)

    @property
    def ready(self) -> bool:
        return self.history.ready and self.harness is not None

    async def ask(self, user_id: str, session_id: str | None, message: str, authorization: str) -> ChatResponse:
        if not self.ready:
            raise RuntimeError("AI service is not configured")
        await self.admission.enter(user_id)
        try:
            resolved_session_id, history = await asyncio.to_thread(
                self.history.start_turn, user_id, session_id, message, self.settings.max_history_messages
            )
            tool_context = ToolContext(
                user_id=user_id,
                authorization=authorization,
                market_client=self.market_client,
                user_client=self.user_client,
                rag_search=self.rag.search if self.rag else None,
                original_request=message,
            )
            try:
                async with asyncio.timeout(self.settings.turn_timeout_seconds):
                    answer = await self.harness.answer(message, history, tool_context)
                await asyncio.to_thread(
                    self.history.save_assistant, user_id, resolved_session_id, answer.text,
                    answer.model, answer.prompt_tokens, answer.completion_tokens,
                )
                return ChatResponse(
                    session_id=resolved_session_id,
                    answer=answer.text,
                    stock_symbols=answer.stock_symbols,
                    article_slugs=answer.article_slugs,
                    references=answer.citations,
                )
            except Exception as exc:
                try:
                    await asyncio.to_thread(self.history.save_failure, user_id, resolved_session_id, type(exc).__name__)
                except Exception:
                    pass
                raise
        finally:
            await self.admission.leave()


class AIService(ai_pb2_grpc.AIServiceServicer):
    def __init__(self, runtime: ChatRuntime, settings: Settings):
        self.runtime = runtime
        self.settings = settings

    async def Ask(self, request: ai_pb2.AIAskRequest, context: grpc.aio.ServicerContext):
        try:
            user_id, authorization = _identity_from_authorization(
                dict(context.invocation_metadata()).get("authorization", ""), self.settings
            )
        except ValueError:
            await context.abort(grpc.StatusCode.UNAUTHENTICATED, "valid bearer token required")
        if request.user_id and request.user_id != user_id:
            await context.abort(grpc.StatusCode.PERMISSION_DENIED, "user identity does not match bearer token")
        if not request.message.strip():
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "message is required")
        try:
            result = await self.runtime.ask(user_id, request.session_id or None, request.message, authorization)
        except LookupError:
            await context.abort(grpc.StatusCode.NOT_FOUND, "chat session not found")
        except RateLimitExceeded:
            await context.abort(grpc.StatusCode.RESOURCE_EXHAUSTED, "chat request rate limit exceeded")
        except ServiceBusy:
            await context.abort(grpc.StatusCode.UNAVAILABLE, "AI service is busy; retry shortly")
        except RuntimeError:
            await context.abort(grpc.StatusCode.FAILED_PRECONDITION, "AI service is not configured")
        except Exception:
            await context.abort(grpc.StatusCode.UNAVAILABLE, "AI response is temporarily unavailable")
        return ai_pb2.AIAskResponse(
            session_id=result.session_id,
            answer=result.answer,
            stock_symbols=result.stock_symbols,
            article_slugs=result.article_slugs,
        )


def _identity_from_authorization(header: str, settings: Settings) -> tuple[str, str]:
    parts = (header or "").split()
    if len(parts) != 2 or parts[0].casefold() != "bearer":
        raise ValueError("missing bearer token")
    if len(settings.jwt_signing_key) < 32:
        raise ValueError("JWT verification is not configured")
    try:
        claims = jwt.decode(
            parts[1], settings.jwt_signing_key,
            algorithms=["HS256"], issuer=settings.jwt_issuer,
            options={"require": ["sub", "iss", "exp"]},
        )
    except jwt.InvalidTokenError as exc:
        raise ValueError("invalid bearer token") from exc
    user_id = str(claims.get("sub") or "").strip()
    if not user_id or len(user_id) > 36:
        raise ValueError("invalid user subject")
    return user_id, f"Bearer {parts[1]}"


async def _authenticated_user(authorization: str | None, settings: Settings) -> tuple[str, str]:
    try:
        return _identity_from_authorization(authorization or "", settings)
    except Exception as exc:
        raise HTTPException(status_code=401, detail="valid bearer token required") from exc


@asynccontextmanager
async def lifespan(app: FastAPI) -> AsyncIterator[None]:
    settings = Settings.from_env()
    history = ChatHistoryStore(settings)
    llm = None
    if settings.llm_ready:
        llm = OpenAICompatibleClient(
            settings.llm_base_url, settings.llm_api_key, settings.llm_model,
            settings.llm_timeout_seconds, settings.max_output_tokens,
        )
    rag = None
    if settings.oracle_ready:
        try:
            rag = RagService(settings)
        except Exception:
            rag = None
    market_channel = grpc.aio.insecure_channel(settings.market_service_addr)
    user_channel = grpc.aio.insecure_channel(settings.user_service_addr)
    try:
        from finsights.v1 import market_pb2_grpc, user_pb2_grpc
        market_client = market_pb2_grpc.MarketServiceStub(market_channel)
        user_client = user_pb2_grpc.UserServiceStub(user_channel)
    except ImportError:
        market_client = None
        user_client = None
    harness = Harness(settings, llm, ToolRegistry()) if llm is not None else None
    runtime = ChatRuntime(settings, history, harness, market_client, user_client, rag)
    app.state.settings = settings
    app.state.runtime = runtime

    grpc_server = grpc.aio.server(options=[
        ("grpc.max_receive_message_length", 1 << 20),
        ("grpc.max_send_message_length", 1 << 20),
    ])
    ai_pb2_grpc.add_AIServiceServicer_to_server(AIService(runtime, settings), grpc_server)
    grpc_addr = os.getenv("AI_GRPC_ADDR", "0.0.0.0:9094")
    if grpc_server.add_insecure_port(grpc_addr) == 0:
        raise RuntimeError(f"could not bind AI gRPC listener at {grpc_addr}")
    await grpc_server.start()
    try:
        yield
    finally:
        await grpc_server.stop(grace=5)
        await market_channel.close()
        await user_channel.close()
        history.close()
        if rag is not None:
            rag.close()


app = FastAPI(title="Finsights AI Service", version="0.2.0", lifespan=lifespan)


def _runtime(app: FastAPI) -> ChatRuntime:
    return app.state.runtime


@app.get("/healthz")
async def healthz() -> dict[str, str]:
    return {"status": "alive"}


@app.get("/readyz")
async def readyz() -> dict[str, str | bool]:
    runtime = _runtime(app)
    if not runtime.ready:
        raise HTTPException(status_code=503, detail="AI LLM and chat-history database are not configured")
    try:
        await asyncio.to_thread(runtime.history.ping)
    except Exception as exc:
        raise HTTPException(status_code=503, detail="AI chat-history database is unavailable") from exc
    rag_ready = False
    if runtime.rag is not None:
        try:
            rag_ready = await runtime.rag.ping()
        except Exception:
            rag_ready = False
    return {"status": "ready", "rag_enabled": runtime.rag is not None, "oracle_available": rag_ready}


@app.post("/v1/chat", response_model=ChatResponse)
async def chat(request: ChatRequest, authorization: str | None = Header(default=None)) -> ChatResponse:
    settings: Settings = app.state.settings
    user_id, bearer = await _authenticated_user(authorization, settings)
    try:
        return await _runtime(app).ask(user_id, request.session_id, request.message.strip(), bearer)
    except LookupError as exc:
        raise HTTPException(status_code=404, detail="chat session not found") from exc
    except RateLimitExceeded as exc:
        raise HTTPException(status_code=429, detail="chat request rate limit exceeded") from exc
    except ServiceBusy as exc:
        raise HTTPException(status_code=503, detail="AI service is busy; retry shortly") from exc
    except RuntimeError as exc:
        raise HTTPException(status_code=503, detail="AI service is not configured") from exc
    except Exception as exc:
        raise HTTPException(status_code=503, detail="AI response is temporarily unavailable") from exc


@app.get("/v1/sessions")
async def list_sessions(limit: int = Query(default=20, ge=1, le=50),
                        offset: int = Query(default=0, ge=0, le=1000),
                        authorization: str | None = Header(default=None)) -> dict:
    settings: Settings = app.state.settings
    user_id, _ = await _authenticated_user(authorization, settings)
    try:
        items = await asyncio.to_thread(_runtime(app).history.list_sessions, user_id, limit, offset)
    except Exception as exc:
        raise HTTPException(status_code=503, detail="chat history is unavailable") from exc
    return {"sessions": items}


@app.get("/v1/sessions/{session_id}/messages")
async def get_messages(session_id: str, limit: int = Query(default=50, ge=1, le=100),
                       authorization: str | None = Header(default=None)) -> dict:
    settings: Settings = app.state.settings
    user_id, _ = await _authenticated_user(authorization, settings)
    try:
        messages = await asyncio.to_thread(_runtime(app).history.get_messages, user_id, session_id, limit)
    except LookupError as exc:
        raise HTTPException(status_code=404, detail="chat session not found") from exc
    except Exception as exc:
        raise HTTPException(status_code=503, detail="chat history is unavailable") from exc
    return {"session_id": session_id, "messages": messages}


@app.delete("/v1/sessions/{session_id}")
async def delete_session(session_id: str, authorization: str | None = Header(default=None)) -> dict[str, bool]:
    settings: Settings = app.state.settings
    user_id, _ = await _authenticated_user(authorization, settings)
    try:
        closed = await asyncio.to_thread(_runtime(app).history.close_session, user_id, session_id)
    except Exception as exc:
        raise HTTPException(status_code=503, detail="chat history is unavailable") from exc
    if not closed:
        raise HTTPException(status_code=404, detail="chat session not found")
    return {"deleted": True}
