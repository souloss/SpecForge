// service-api 入口：chi v4（github.com/go-chi/chi，无 /v5）路由挂载。
package main

import (
	"net/http"

	"example.com/chi4repo/internal/api"

	"github.com/go-chi/chi"
)

func main() {
	r := chi.NewRouter()
	api.Register(r)
	_ = http.ListenAndServe(":8080", r)
}
