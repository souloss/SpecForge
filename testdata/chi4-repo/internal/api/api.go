// Package api chi v4 适配器验证 fixture：覆盖回调式 Route/Group、Method 注册、
// chi.URLParam 路径参数、url.Values.Get 查询参数、json Decoder 绑定与 json Encoder 写出。
//
// 与 testdata/chi-repo（chi v5）同构，仅 import 路径换成无 /v5 的 v4，
// 用于验证内置 chi v4 适配器是否开箱即用。
package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi"
)

// CreateUserRequest 创建用户请求体。
type CreateUserRequest struct {
	Name string `json:"name"`
}

// User 用户响应体。
type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// registerOptions 路由注册选项（覆盖动态拼接路径的 Register 变体）。
type registerOptions struct {
	BaseURL string
}

// Register 注册全部路由：Route 回调、Group、Method 注册、URLParam/查询参数/JSON 绑定写出。
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

// GetUser 读取单个用户：URLParam 路径参数 + 查询参数 + JSON 写出。
func GetUser(w http.ResponseWriter, r *http.Request) {
	_ = chi.URLParam(r, "id")
	_ = r.URL.Query().Get("expand")
	_ = json.NewEncoder(w).Encode(User{ID: "1", Name: "Ada"})
}

// CreateUser 创建用户：JSON 请求体绑定 + 400 错误分支 + JSON 写出。
func CreateUser(w http.ResponseWriter, r *http.Request) {
	var req CreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(User{Name: req.Name})
}

// Stats 统计：map 字面量 JSON 写出（additionalProperties 形态）。
func Stats(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]int{"total": 1})
}
