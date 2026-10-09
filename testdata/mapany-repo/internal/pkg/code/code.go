// Package code 响应写出口与业务错误（统一写出口：任何 data 原样进入信封 data 槽）。
package code

import "github.com/gofiber/fiber/v2"

// 业务错误码。
const (
	ErrSuccess     int = 0
	ErrBadRequest  int = 1001
	ErrUnauthorized int = 1002
	ErrNotFound    int = 1003
)

// Error 带业务码的错误。
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error 返回错误信息。
func (e *Error) Error() string { return e.Message }

// NewError 构造业务错误。
func NewError(code int, msg string) *Error { return &Error{Code: code, Message: msg} }

// Envelope 响应信封：data 槽承载业务体（any）。
type Envelope struct {
	Code int `json:"code"`
	Msg  string `json:"msg"`
	Data any `json:"data"`
}

// Response 统一写出：err 非空时写错误码，否则写成功码；data 原样进入 data 槽。
func Response(c *fiber.Ctx, err error, data any) error {
	if err != nil {
		if e, ok := err.(*Error); ok {
			return c.JSON(Envelope{Code: e.Code, Msg: e.Message, Data: map[string]any{}})
		}
		return c.JSON(Envelope{Code: ErrBadRequest, Msg: err.Error(), Data: map[string]any{}})
	}
	return c.JSON(Envelope{Code: ErrSuccess, Msg: "ok", Data: data})
}
