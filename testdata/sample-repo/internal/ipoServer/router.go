// Package ipoServer 路由注册（对应 trade/internal/ipoServer/router.go）。
//
// 包含全部难点模式: 通配符组前缀、三层中间件链、表驱动循环注册。
package ipoServer

import (
	ipocontroller "trade.local/repo/internal/ipoServer/controller/ipoServer"
	"trade.local/repo/internal/ipoServer/mapping"
	"trade.local/repo/internal/pkg/auth"
	"trade.local/repo/internal/pkg/code"
	"trade.local/repo/internal/pkg/subaccount"

	"github.com/gofiber/fiber/v2"
)

// HealthController 健康检查控制器。
type HealthController struct {
	Version string
}

// Health 健康检查。
func (h *HealthController) Health(c *fiber.Ctx) error {
	return code.WriteResponse(c, nil, &mapping.HealthStatus{Status: "ok", Version: h.Version})
}

// RegisterRoutes 注册全部路由。
func RegisterRoutes(app *fiber.App, ctl *ipocontroller.IpoController, hc *HealthController) {
	ipoServers := app.Group("/ipo/v*")

	ipoServers.Post("/OrderCheck",
		auth.GetUserRelation(), subaccount.NewSubAccountIntercepter(), auth.PermissionCheck("0xff"),
		ctl.OrderCheck)

	ipoServers.Post("/OrderCreate",
		auth.GetUserRelation(), auth.PermissionCheck("0xff"),
		ctl.OrderCreate)

	ipoServers.Post("/OrderQuery",
		auth.GetUserRelation(),
		ctl.OrderQuery)

	ipoServers.Get("/orders/:orderId",
		auth.GetUserRelation(),
		ctl.GetOrderDetail)

	ipoServers.Get("/orders/:orderId/items",
		auth.GetUserRelation(),
		ctl.ListOrderItems)

	ipoServers.Post("/OrderCancel",
		auth.GetUserRelation(), auth.PermissionCheck("0x01"),
		ctl.OrderCancel)

	ipoServers.Get("/controls/assess",
		auth.GetUserRelation(), subaccount.NewSubAccountIntercepter(),
		ctl.RiskAssess)

	app.Get("/health", hc.Health)

	// 表驱动循环注册（legacy 兼容路由）。
	legacy := []struct {
		Path    string
		Handler fiber.Handler
	}{
		{Path: "/legacy/v1/quote", Handler: ctl.LegacyQuote},
		{Path: "/legacy/v1/depth", Handler: ctl.LegacyDepth},
	}
	for _, r := range legacy {
		app.Get(r.Path, r.Handler)
	}
}
