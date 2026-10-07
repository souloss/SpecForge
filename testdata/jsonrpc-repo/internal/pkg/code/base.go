// Package code JSON-RPC 2.0 响应信封与业务错误（对应 sample-ipo/pkg/code）。
package code

import (
	"fmt"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
)

// HeaderRequestId 请求 ID 头名（真实仓库为小写 "X-request-id"，与 shared-lib canonical "X-Request-Id" 并存）。
const HeaderRequestId = "X-request-id"

// HeaderRetCode 响应头携带业务码（成功 "0"，失败为错误码字符串）。
const HeaderRetCode = "X-retcode"

// JsonRpc 协议版本。
const JsonRpc = "2.0"

// Error 带业务码的错误（错误对象：code/message/data）。
type Error struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data"`
}

// Error 返回业务码字符串——这是 X-retcode 响应头的值来源。
func (e *Error) Error() string { return fmt.Sprint(e.Code) }

// 业务错误码常量（错误码目录的静态真源）。
const (
	ErrSuccess       int = 0
	ErrUnknow        int = 100001
	ErrRequest       int = 100002
	ErrPermission    int = 100009
	ErrInvalidBody   int = 100012
	ErrInvalidParams int = 100013
	ErrNotFound      int = 100106
)

var errorMessage = map[int]string{
	ErrInvalidBody:   "invalid request body",
	ErrInvalidParams: "invalid params",
	ErrPermission:    "permission denied",
	ErrNotFound:      "resource not found",
}

// NewError 构造业务错误。
func NewError(code int, msg string, data interface{}) *Error {
	if data == nil {
		return &Error{Code: code, Message: msg, Data: map[string]string{}}
	}
	return &Error{Code: code, Message: msg, Data: data}
}

// NewDefaultError 按错误码查表构造业务错误。
func NewDefaultError(code int) *Error {
	return &Error{Code: code, Message: errorMessage[code], Data: map[string]string{}}
}

// ErrResponse 失败信封（JSON-RPC 2.0）。
type ErrResponse struct {
	Jsonrpc string      `json:"jsonrpc"`
	Result  interface{} `json:"result,omitempty"`
	Error   *Error      `json:"error"`
	Id      string      `json:"id"`
}

// BaseData 请求/响应的公共外壳字段。
type BaseData struct {
	JsonRpc string `json:"jsonrpc" validate:"required,eq=2.0"`
	Id      string `json:"id" validate:"required"`
}

// BaseResp 成功信封（JSON-RPC 2.0）：result 承载业务数据。
type BaseResp struct {
	BaseData
	Result interface{} `json:"result"`
	Error  interface{} `json:"error,omitempty"`
}

// requestID 取上下文里的请求 ID，无则退回毫秒时间戳（Response 的 fallback 语义）。
func requestID(c *fiber.Ctx) string {
	if v, ok := c.Context().UserValue(HeaderRequestId).(string); ok {
		return v
	}
	return strconv.Itoa(int(time.Now().UnixMilli()))
}

// WriteResponse 成功**裸写** data（无信封），失败写 ErrResponse——与 Response 的成功形态不同。
func WriteResponse(c *fiber.Ctx, err error, data interface{}) error {
	var id string
	if v, ok := c.Context().UserValue(HeaderRequestId).(string); ok {
		id = v
	}
	if err != nil {
		c.Set(HeaderRetCode, err.Error())
		return c.JSON(ErrResponse{Jsonrpc: JsonRpc, Error: err, Id: id})
	}
	c.Set(HeaderRetCode, "0")
	return c.JSON(data)
}

// Response 成功包 {jsonrpc,id,result} 信封，失败写 ErrResponse。
func Response(c *fiber.Ctx, err error, data interface{}) error {
	if err != nil {
		c.Set(HeaderRetCode, err.Error())
		return c.JSON(ErrResponse{Jsonrpc: JsonRpc, Error: err, Id: requestID(c)})
	}
	if data == nil {
		data = map[string]interface{}{}
	}
	c.Set(HeaderRetCode, "0")
	return c.JSON(&BaseResp{BaseData: BaseData{Id: requestID(c), JsonRpc: JsonRpc}, Result: data})
}
