// Command api 启动 gorilla/mux 样本服务（无内置适配器，验证路由抽取缺口）。
package main

import (
	"net/http"

	"example.com/gorillarepo/internal/api"

	"github.com/gorilla/mux"
)

func main() {
	r := mux.NewRouter()
	api.Register(r)
	_ = http.ListenAndServe(":8080", r)
}
