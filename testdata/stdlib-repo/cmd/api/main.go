// Command api 启动标准库 net/http 样本服务：Go 1.22+ ServeMux 方法+路径模式路由。
//
// 本仓库刻意不使用 Fiber/Gin/chi/echo 等第三方框架，仅依赖标准库，
// 用于验证引擎对「net/http ServeMux 路由」这一未内置适配器形态的行为
// （预期：0 条路由被静态抽取，需记录 engine 缺口；LLM 前端对 Go 仓库也不走 generic）。
package main

import (
	"log"
	"net/http"

	"example.com/stdlibrepo/internal/api"
)

func main() {
	mux := http.NewServeMux()
	api.Register(mux)
	log.Fatal(http.ListenAndServe(":8080", mux))
}
