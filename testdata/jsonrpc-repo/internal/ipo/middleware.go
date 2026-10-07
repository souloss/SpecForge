// Package ipo 鉴权中间件（对应 sample-ipo/middleware/auth）。
//
// 真实仓库的鉴权中间件读自定义 header（X-uin/X-session/X-request-id），
// 并把用户关系写入 c.Locals；无 token/JWT。这里按 profile 的 auth_middleware
// 映射暴露 header 参数与 security scheme。
package ipo

import (
	"trade.local/jsonrpc/internal/pkg/code"

	"github.com/gofiber/fiber/v2"
)

// HeaderUin 用户标识头。
const HeaderUin = "X-uin"

// getUserRelation 用户身份中间件：强制 X-uin 头，非空才放行。
func getUserRelation() fiber.Handler {
	return func(c *fiber.Ctx) error {
		uin := c.Get(HeaderUin)
		if uin == "" {
			return code.Response(c, code.NewDefaultError(code.ErrPermission), nil)
		}
		c.Locals(HeaderUin, uin)
		return c.Next()
	}
}

// permissionCheck 权限位校验中间件（scope 字面量实参，真实仓库校验权限点）。
func permissionCheck(scope string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		_ = scope
		return c.Next()
	}
}
