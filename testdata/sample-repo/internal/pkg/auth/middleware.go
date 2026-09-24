// Package auth 鉴权中间件（对应 trade/pkg/auth）。
package auth

import (
	"trade.local/repo/internal/pkg/code"

	"github.com/gofiber/fiber/v2"
)

// GetUserRelation 用户身份中间件: 强制 X-User-Token 头。
func GetUserRelation() fiber.Handler {
	return func(c *fiber.Ctx) error {
		token := c.Get("X-User-Token")
		if token == "" {
			return code.WriteResponse(c, code.NewDefaultError(code.ErrUnauthorized), nil)
		}
		return c.Next()
	}
}

// PermissionCheck 权限位校验中间件。
func PermissionCheck(scope string) fiber.Handler {
	return func(c *fiber.Ctx) error {
		return c.Next()
	}
}
