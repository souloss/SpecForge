package main

import (
	"net/http"

	"example.com/chirepo/internal/api"
	"github.com/go-chi/chi/v5"
)

func main() {
	r := chi.NewRouter()
	api.Register(r)
	_ = http.ListenAndServe(":8080", r)
}
