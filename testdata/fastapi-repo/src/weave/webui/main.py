# FastAPI 应用工厂：集中 include_router 注册 + app.state 依赖注入（对应 weave/webui/main.py）。
from fastapi import FastAPI

from .routes import auth, runs


def create_app() -> FastAPI:
    app = FastAPI(title="Weave", version="0.1.0")
    app.include_router(auth.router)
    app.include_router(runs.router)

    @app.get("/api/health", tags=["system"])
    async def health() -> dict[str, str]:
        return {"status": "ok"}

    return app
