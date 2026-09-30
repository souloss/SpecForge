// Package code 响应信封与业务错误（仿 sample-app：错误对象原样进入信封 error 字段）。
package code

import "github.com/gofiber/fiber/v2"

// 业务错误码。
const (
	ErrNotFound int = 100404
	ErrLocked   int = 100423
)

// Error 带业务码的错误。
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// NewError 构造业务错误。
func NewError(code int, msg string) *Error { return &Error{Code: code, Message: msg} }

// ErrResponse 失败信封。
type ErrResponse struct {
	Error error `json:"error"`
}

// OkResponse 成功信封。
type OkResponse struct {
	Result any `json:"result"`
}

// Response 统一写出：err 原样进入 ErrResponse.Error。
func Response(c *fiber.Ctx, err error, data any) error {
	if err != nil {
		return c.JSON(ErrResponse{Error: err})
	}
	return c.JSON(OkResponse{Result: data})
}
