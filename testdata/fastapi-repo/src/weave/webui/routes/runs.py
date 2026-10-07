# 任务路由：Pydantic 请求体 + 路径参数 + 状态码 + SSE 流（对应 weave/webui/routes/runs.py）。
import json
from typing import Any

from fastapi import APIRouter, HTTPException, Request, status
from fastapi.responses import StreamingResponse
from pydantic import BaseModel, Field

router = APIRouter(prefix="/api/runs", tags=["runs"])


class SubmitRunRequest(BaseModel):
    suite_id: str = Field(pattern=r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$")
    case_ids: list[str] = Field(default_factory=list, max_length=500)
    run_id: str | None = Field(default=None, pattern=r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$")


@router.post("/submit", status_code=status.HTTP_202_ACCEPTED)
async def submit_run(payload: SubmitRunRequest) -> dict[str, str]:
    return {"run_id": "run-1", "status": "queued", "suite_id": payload.suite_id, "cases": str(len(payload.case_ids))}


@router.get("/{run_id}")
async def get_run(run_id: str) -> dict[str, Any]:
    if run_id == "missing":
        raise HTTPException(status.HTTP_404_NOT_FOUND, "run not found")
    return {"run_id": run_id, "status": "done"}


@router.get("/{run_id}/events")
async def run_events(run_id: str, request: Request) -> StreamingResponse:
    async def stream():
        yield f"id: 1\nevent: run\ndata: {json.dumps({'run_id': run_id})}\n\n"

    return StreamingResponse(stream(), media_type="text/event-stream")
