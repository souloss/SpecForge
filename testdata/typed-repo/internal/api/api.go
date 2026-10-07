// Package api 强类型 RESTful 响应对象（对应 meridian 的 oapi-codegen strict-server 形态的手写同构）。
//
// 契约点：
//   - 成功响应体即业务结构体，状态码藏在命名响应类型里（200/201/204）；
//   - 错误是稳定信封 {code,message,requestId}，code 为蛇形字符串枚举，非整数业务码；
//   - 请求解码走 json.NewDecoder(r.Body).Decode，无 binding tag；
//   - HTTP 状态是真实 RESTful 状态码（非恒 200）。
package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// ErrorCode 错误码字符串枚举。
type ErrorCode string

// 错误码常量（对应 meridian/service/error_codes.go）。
const (
	CodeUnauthenticated ErrorCode = "unauthenticated"
	CodeNotFound        ErrorCode = "not_found"
	CodeValidation      ErrorCode = "validation_error"
	CodeDuplicate       ErrorCode = "duplicate"
)

// LoginRequest 登录请求体。
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginResult 登录结果（成功业务体）。
type LoginResult struct {
	AccessToken      string `json:"accessToken"`
	ExpiresInSeconds int    `json:"expiresInSeconds"`
}

// User 用户（创建/查询的业务体）。
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

// Asset 资产（查询的业务体）。
type Asset struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CreateUserRequest 创建用户请求体。
type CreateUserRequest struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

// Register 注册路由。
func Register(r chi.Router) {
	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/auth/login", Login)
		r.Post("/users", CreateUser)
		r.Get("/assets/{assetId}", GetAsset)
		r.Delete("/assets/{assetId}", DeleteAsset)
	})
}

// errUnauthenticated 鉴权失败哨兵。
var errUnauthenticated = errors.New("bad credentials")

// Login 登录（成功 200 + LoginResult）。
func Login(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, CodeValidation, nil)
		return
	}
	if req.Password == "" {
		writeError(w, r, http.StatusUnauthorized, CodeUnauthenticated, nil)
		return
	}
	writeJSON(w, http.StatusOK, LoginResult{AccessToken: "tok-1", ExpiresInSeconds: 3600})
}

// CreateUser 创建用户（成功 201 + User）。
func CreateUser(w http.ResponseWriter, r *http.Request) {
	var req CreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, r, http.StatusUnprocessableEntity, CodeValidation, nil)
		return
	}
	if req.Username == "" {
		writeError(w, r, http.StatusUnprocessableEntity, CodeValidation, nil)
		return
	}
	writeJSON(w, http.StatusCreated, User{ID: "u-1", Username: req.Username, Role: req.Role})
}

// GetAsset 查询资产（成功 200 + Asset，404 分支）。
func GetAsset(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "assetId")
	if id == "missing" {
		writeError(w, r, http.StatusNotFound, CodeNotFound, nil)
		return
	}
	writeJSON(w, http.StatusOK, Asset{ID: id, Name: "asset-" + id})
}

// DeleteAsset 删除资产（成功 204 无 body，409 分支）。
func DeleteAsset(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "assetId")
	if id == "locked" {
		writeError(w, r, http.StatusConflict, CodeDuplicate, nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
