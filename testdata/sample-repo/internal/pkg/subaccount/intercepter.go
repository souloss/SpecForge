// Package subaccount 子账户拦截中间件（对应 trade/pkg/subaccount）。
package subaccount

import (
	"trade.local/repo/internal/pkg/code"

	"github.com/gofiber/fiber/v2"
)

// NewSubAccountIntercepter 可选的 X-Sub-Account-Id 头中间件。
func NewSubAccountIntercepter() fiber.Handler {
	return func(c *fiber.Ctx) error {
		sub := c.Get("X-Sub-Account-Id")
		if sub != "" && !validSubAccount(sub) {
			return code.WriteResponse(c, code.NewDefaultError(code.ErrSubAccountDeny), nil)
		}
		return c.Next()
	}
}

func validSubAccount(s string) bool { return len(s) <= 32 }
