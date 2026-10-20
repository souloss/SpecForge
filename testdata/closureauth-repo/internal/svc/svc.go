// Package svc 闭包中间件读取 header 的用例。
//
// 契约点：路由经 checkPerm() 闭包中间件注册，闭包返回内层 requireAuth()，
// requireAuth 读 X-user 头并在缺失时短路返回 401。header 读取发生在闭包的内层
// 函数，静态前端沿「中间件符号」的正向可达闭包追不到它——X-user 头被静默漏掉
// （既不产出 header 参数，也不报缺口）。见 PATTERNS.md §8。
package svc

import (
	"closureauth.local/repo/internal/pkg/code"

	"github.com/gofiber/fiber/v2"
)

// HeaderUser 请求头名。
const HeaderUser = "X-user"

// RegisterRoutes 注册路由：三条路由都走同一闭包中间件 checkPerm()。
func RegisterRoutes(app *fiber.App) {
	checkPerm := func() fiber.Handler {
		return requireAuth()
	}
	app.Get("/account", checkPerm(), getAccount)
	app.Get("/settings", checkPerm(), getSettings)
	app.Get("/profile", checkPerm(), getProfile)
}

// requireAuth 读 X-user 头，缺失时短路返回 401。
func requireAuth() fiber.Handler {
	return func(c *fiber.Ctx) error {
		u := c.Get(HeaderUser)
		if u == "" {
			return code.Response(c, code.NewError(401, "missing user"), nil)
		}
		c.Locals("user", u)
		return c.Next()
	}
}

// getAccount 返回账户信息。
func getAccount(c *fiber.Ctx) error {
	return code.Response(c, nil, map[string]any{"name": "a", "balance": 1})
}

// getSettings 返回设置信息。
func getSettings(c *fiber.Ctx) error {
	return code.Response(c, nil, map[string]any{"theme": "dark", "locale": "zh"})
}

// getProfile 返回画像信息。
func getProfile(c *fiber.Ctx) error {
	return code.Response(c, nil, map[string]any{"age": 30, "vip": true})
}
