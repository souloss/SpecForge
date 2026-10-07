// service-api 入口。
package main

import (
	"net/http"

	"example.com/typedrepo/internal/api"

	"github.com/go-chi/chi/v5"
)

func main() {
	r := chi.NewRouter()
	api.Register(r)
	_ = http.ListenAndServe(":8080", r)
}
