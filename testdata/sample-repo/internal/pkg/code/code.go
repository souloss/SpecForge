// Package code 统一响应信封与业务错误码（对应 trade/pkg/code）。
package code

import (
	"github.com/gofiber/fiber/v2"
)

// Code 业务错误码类型。
type Code int

// 错误码常量（错误码目录的静态真源）。
const (
	ErrInvalidBody   Code = 40001
	ErrValidateFail  Code = 40002
	ErrUnauthorized  Code = 43001
	ErrOrderClosed   Code = 41001
	ErrAmountExceed  Code = 41002
	ErrNotFound      Code = 42001
	ErrOrderExists   Code = 41003
	ErrSubAccountDeny Code = 43101
)

var errMessages = map[Code]string{
	ErrInvalidBody:    "invalid request body",
	ErrValidateFail:   "validation failed",
	ErrUnauthorized:   "unauthorized",
	ErrOrderClosed:    "order already closed",
	ErrAmountExceed:   "order amount exceeds limit",
	ErrNotFound:       "resource not found",
	ErrOrderExists:    "duplicate order exists",
	ErrSubAccountDeny: "sub-account not permitted",
}

// BizError 业务错误。
type BizError struct {
	Code Code
	Msg  string
}

func (e *BizError) Error() string { return e.Msg }

// NewDefaultError 构造业务错误。
func NewDefaultError(c Code) error {
	return &BizError{Code: c, Msg: errMessages[c]}
}

// NewBizError 带覆盖消息构造业务错误。
func NewBizError(c Code, msg string) error {
	return &BizError{Code: c, Msg: msg}
}

// WriteResponse 统一响应汇聚点: HTTP 200 + 业务码信封。
func WriteResponse(c *fiber.Ctx, err error, data any) error {
	if err != nil {
		be, ok := err.(*BizError)
		if !ok {
			be = &BizError{Code: ErrInvalidBody, Msg: err.Error()}
		}
		return c.JSON(fiber.Map{"code": be.Code, "msg": be.Msg})
	}
	return c.JSON(fiber.Map{"code": 0, "msg": "success", "data": data})
}
