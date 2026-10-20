// Package svc header 参数名以常量引用给出的用例。
//
// 契约点：handler 读 c.Get(HeaderTrace)/c.Get(HeaderRegion)，HeaderTrace/HeaderRegion
// 是字符串常量（值 "X-Trace"/"X-Region"）。通用 LLM 前端无类型信息，只看到标识符
// 本身，会把常量名当参数名输出（真实参数名应是常量的值）。见 PATTERNS.md §10。
package svc

import "github.com/gofiber/fiber/v2"

// HeaderTrace 请求头名。
const HeaderTrace = "X-Trace"

// HeaderRegion 请求头名。
const HeaderRegion = "X-Region"

// RegisterRoutes 注册路由。
func RegisterRoutes(app *fiber.App) {
	app.Get("/trace", trace)
	app.Get("/region", region)
	app.Get("/both", both)
}

// trace 读 X-Trace 头。
func trace(c *fiber.Ctx) error {
	_ = c.Get(HeaderTrace)
	return c.JSON(map[string]any{"ok": true})
}

// region 读 X-Region 头。
func region(c *fiber.Ctx) error {
	_ = c.Get(HeaderRegion)
	return c.JSON(map[string]any{"ok": true})
}

// both 读两个头。
func both(c *fiber.Ctx) error {
	_ = c.Get(HeaderTrace)
	_ = c.Get(HeaderRegion)
	return c.JSON(map[string]any{"ok": true})
}
