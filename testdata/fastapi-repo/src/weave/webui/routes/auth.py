# 鉴权路由：RESTful 直接返回 dict + HTTPException（对应 weave/webui/routes/auth.py）。
from typing import Annotated

from fastapi import APIRouter, Cookie, Header, HTTPException, Response, status
from pydantic import BaseModel, Field

router = APIRouter(prefix="/api/auth", tags=["auth"])


class LoginRequest(BaseModel):
    username: str = Field(min_length=1, max_length=128)
    password: str = Field(min_length=1, max_length=128)


@router.post("/login")
async def login(payload: LoginRequest, response: Response) -> dict:
    if payload.password != "secret":
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, "invalid username or password")
    response.set_cookie("weave_session", "sess-1")
    return {"id": "u-1", "username": payload.username, "role": "admin"}


@router.get("/me")
async def me(
    session: Annotated[str | None, Cookie(alias="weave_session")] = None,
    x_csrf: Annotated[str | None, Header(alias="X-CSRF-Token")] = None,
) -> dict:
    if not session:
        raise HTTPException(status.HTTP_401_UNAUTHORIZED, "authentication required")
    _ = x_csrf
    return {"id": "u-1", "username": "alice", "role": "admin"}
