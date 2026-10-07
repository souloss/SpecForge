// Package code JSON-RPC 信封变体（对应 access-control/pkg/code）：错误对象字段名是 msg（非 message）。
package code

import (
	"fmt"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
)

// HeaderRequestId 请求 ID 头名。
const HeaderRequestId = "X-request-id"

// HeaderRetCode 响应头业务码。
const HeaderRetCode = "X-retcode"

// JsonRpc 协议版本。
const JsonRpc = "2.0"

// Error 带业务码的错误（字段名 msg，非 message——契约变体点）。
type Error struct {
	Code int         `json:"code"`
	Msg  string      `json:"msg"`
	Data interface{} `json:"data"`
}

// Error 返回业务码字符串（X-retcode 值来源）。
func (e *Error) Error() string { return fmt.Sprint(e.Code) }

// 业务错误码常量。
const (
	ErrSuccess      int = 0
	ErrUnknow       int = 100001
	ErrInvalidBody  int = 100012
	ErrPermission   int = 100009
	ErrRoleNotFound int = 100201
)

var errorMessage = map[int]string{
	ErrInvalidBody:  "invalid request body",
	ErrPermission:   "permission denied",
	ErrRoleNotFound: "role not found",
}

// NewError 构造业务错误（Data nil 时省略，与 sample-ipo 的 map[string]string{} 不同）。
func NewError(code int, msg string, data interface{}) *Error {
	if data == nil {
		return &Error{Code: code, Msg: msg}
	}
	return &Error{Code: code, Msg: msg, Data: data}
}

// NewDefaultError 按错误码查表。
func NewDefaultError(code int) *Error {
	return &Error{Code: code, Msg: errorMessage[code]}
}

// ErrResponse 失败信封（error 为 *Error）。
type ErrResponse struct {
	Jsonrpc string      `json:"jsonrpc"`
	Result  interface{} `json:"result,omitempty"`
	Error   *Error      `json:"error"`
	Id      string      `json:"id"`
}

// BaseData 公共外壳字段。
type BaseData struct {
	JsonRpc string `json:"jsonrpc" validate:"required,eq=2.0"`
	Id      string `json:"id" validate:"required"`
}

// BaseResp 成功信封（由 service 层 BuildBaseResponse 组装后交给 WriteResponse 裸写）。
type BaseResp struct {
	BaseData
	Result interface{} `json:"result"`
	Error  interface{} `json:"error,omitempty"`
}

// WriteResponse 成功裸写 data（data 已由 service 层包好信封），失败写 ErrResponse。
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

// requestID 上下文请求 ID，无则退回时间戳。
func requestID(c *fiber.Ctx) string {
	if v, ok := c.Context().UserValue(HeaderRequestId).(string); ok {
		return v
	}
	return strconv.Itoa(int(time.Now().UnixMilli()))
}

// BuildBaseResponse 组装成功信封（service 层调用，controller 只做裸写）。
func BuildBaseResponse(c *fiber.Ctx, result interface{}) *BaseResp {
	return &BaseResp{BaseData: BaseData{Id: requestID(c), JsonRpc: JsonRpc}, Result: result}
}
