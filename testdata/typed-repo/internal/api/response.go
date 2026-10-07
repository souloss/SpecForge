// Package api 响应写出口：writeJSON 裸写业务体、writeError 写稳定错误信封。
package api

import (
	"encoding/json"
	"net/http"
)

// errorEnvelope 错误响应信封（对应 meridian/handler/response.go 的 writeErrorDetails）。
type errorEnvelope struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	Request string    `json:"requestId"`
}

// errorMessages 错误码 → 消息。
var errorMessages = map[ErrorCode]string{
	CodeUnauthenticated: "authentication required",
	CodeNotFound:        "resource not found",
	CodeValidation:      "validation failed",
	CodeDuplicate:       "resource already exists",
}

// writeJSON 写 JSON 响应体并设置 Content-Type。
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeError 写稳定错误信封。
func writeError(w http.ResponseWriter, r *http.Request, status int, code ErrorCode, _ map[string]any) {
	_ = r
	writeJSON(w, status, errorEnvelope{Code: code, Message: errorMessages[code], Request: "req-1"})
}
