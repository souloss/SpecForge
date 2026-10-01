package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type CreateUserRequest struct {
	Name string `json:"name"`
}

type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type registerOptions struct {
	BaseURL string
}

func Register(r chi.Router) {
	options := registerOptions{}
	r.Route("/api", func(r chi.Router) {
		r.Get("/users/{id}", GetUser)
		r.Method("GET", "/method", GetUser)
		r.Post("/users", CreateUser)
		r.Group(func(r chi.Router) {
			r.Get("/stats", Stats)
		})
	})
	r.Get(options.BaseURL+"/base-url", Stats)
}

func GetUser(w http.ResponseWriter, r *http.Request) {
	_ = chi.URLParam(r, "id")
	_ = r.URL.Query().Get("expand")
	_ = json.NewEncoder(w).Encode(User{ID: "1", Name: "Ada"})
}

func CreateUser(w http.ResponseWriter, r *http.Request) {
	var req CreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(User{Name: req.Name})
}

func Stats(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]int{"total": 1})
}
