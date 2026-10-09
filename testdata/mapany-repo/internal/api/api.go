// Package api 直接 map 响应与中间件响应写法（对应真实仓库中以 map[string]any 直写 + 鉴权中间件短路返回的组合）。
//
// 契约点：
//   - 多个 handler 用 map[string]any 字面量直接 c.JSON 返回不同形状的成功体；
//   - 中间件 authRequired 短路返回固定错误体（读 X-Auth-Token 头，缺失即写错误信封）；
//   - 一个 handler 用结构体转 map（反射）再返回；
//   - 含 header（X-Trace-Id）、cookie（session）参数读取。
package api

import (
	"reflect"
	"time"

	"mapany.local/repo/internal/pkg/code"

	"github.com/gofiber/fiber/v2"
)

// Order 订单业务体（结构体转 map 的源类型）。
type Order struct {
	OrderID string `json:"orderId"`
	Symbol  string `json:"symbol"`
	Qty     int64  `json:"qty"`
	Price   float64 `json:"price"`
}

// RegisterRoutes 注册路由：/meta、/users/:id、/orders 为直接注册；/legacy 组走 authRequired 中间件。
func RegisterRoutes(app *fiber.App) {
	app.Get("/meta", meta)
	app.Get("/users/:id", getUser)
	app.Post("/orders", createOrder)
	legacy := app.Group("/legacy")
	legacy.Get("/ping", authRequired, legacyPing)
	legacy.Get("/health", authRequired, legacyHealth)
}

// authRequired 鉴权中间件：缺 X-Auth-Token 头时短路返回固定错误体（401）。
func authRequired(c *fiber.Ctx) error {
	token := c.Get("X-Auth-Token")
	if token == "" {
		return code.Response(c, code.NewError(code.ErrUnauthorized, "missing auth token"), nil)
	}
	c.Locals("authToken", token)
	return c.Next()
}

// meta 服务元信息：直接 map 字面量返回（含嵌套 map 与 bool/string/int）。
func meta(c *fiber.Ctx) error {
	_ = c.Cookies("session")
	return c.JSON(map[string]any{
		"name":    "mapany-svc",
		"version": 2,
		"healthy": true,
		"info": map[string]any{
			"region": "cn-south",
		},
	})
}

// getUser 查询用户：读 path 参数 id 与 header X-Trace-Id，直接 map 字面量返回。
func getUser(c *fiber.Ctx) error {
	id := c.Params("id")
	_ = c.Get("X-Trace-Id")
	return c.JSON(map[string]any{
		"id":   id,
		"name": "user-" + id,
		"age":  30,
	})
}

// createOrder 下单：请求体解析到 Order 结构体，经反射转 map 后再返回（结构体转 map 写法）。
func createOrder(c *fiber.Ctx) error {
	var req Order
	if err := c.BodyParser(&req); err != nil {
		return code.Response(c, code.NewError(code.ErrBadRequest, "invalid body"), nil)
	}
	return c.JSON(structToMap(req))
}

// legacyPing 探活（legacy 组，经 authRequired 后到达）：直接 map 字面量返回。
func legacyPing(c *fiber.Ctx) error {
	return c.JSON(map[string]any{
		"pong": true,
		"ts":   time.Now().UnixMilli(),
	})
}

// legacyHealth 健康检查（legacy 组）：直接 map 字面量返回。
func legacyHealth(c *fiber.Ctx) error {
	return c.JSON(map[string]any{
		"status":  "up",
		"version": 1,
	})
}

// structToMap 结构体反射转 map[string]any（json 标签名作为键）。
func structToMap(v any) map[string]any {
	out := map[string]any{}
	rv := reflect.ValueOf(v)
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		key := rt.Field(i).Tag.Get("json")
		if key == "" {
			key = rt.Field(i).Name
		}
		out[key] = rv.Field(i).Interface()
	}
	return out
}
