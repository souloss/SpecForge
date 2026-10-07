// Package ipo IPO 控制器与路由（对应 sample-ipo/internal/ipoServer）。
package ipo

import (
	"time"

	"trade.local/jsonrpc/internal/ipo/mapping"
	"trade.local/jsonrpc/internal/pkg/code"

	"github.com/gofiber/fiber/v2"
)

// RegisterRoutes 注册 JSON-RPC 路由（通配版本组 + 逐条内联鉴权中间件）。
func RegisterRoutes(app *fiber.App) {
	app.Get("/Time", ts())
	app.Get("/ipo/v1/Ping", ping())

	ipoServers := app.Group("/ipo/v*")
	ipoServers.Post("/OrderCheck", getUserRelation(), permissionCheck("0xff"), orderCheck)
	ipoServers.Post("/OrderCancel", getUserRelation(), permissionCheck("0xff"), orderCancel)
	ipoServers.Get("/OrderList", getUserRelation(), orderList)
	ipoServers.Post("/StockCreate", getUserRelation(), equityCreate)
}

// orderCheck 下单合规检查（走 Response：成功包 jsonrpc 信封）。
func orderCheck(c *fiber.Ctx) error {
	r := new(mapping.OrderCheckReq)
	if err := c.BodyParser(r); err != nil {
		return code.Response(c, code.NewDefaultError(code.ErrInvalidBody), nil)
	}
	if r.Params.Amount <= 0 {
		return code.Response(c, code.NewDefaultError(code.ErrInvalidParams), nil)
	}
	fee := r.Params.Amount * 0.001
	resp := &mapping.OrderCheckResp{
		OrderID: r.Params.OrderID,
		Status:  "pending",
		Amount:  r.Params.Amount,
		Fee:     &fee,
	}
	return code.Response(c, nil, resp)
}

// orderCancel 撤单（走 Response；业务码在 service 内产生）。
func orderCancel(c *fiber.Ctx) error {
	r := new(mapping.OrderCancelReq)
	if err := c.BodyParser(r); err != nil {
		return code.Response(c, code.NewDefaultError(code.ErrInvalidBody), nil)
	}
	if r.Params.OrderID == "closed" {
		return code.Response(c, code.NewDefaultError(code.ErrNotFound), nil)
	}
	resp := &mapping.OrderCancelResp{OrderID: r.Params.OrderID, Canceled: true, Timestamp: time.Now().UnixMilli()}
	return code.Response(c, nil, resp)
}

// orderList 订单列表（GET，query 平铺；走 WriteResponse 裸写切片）。
func orderList(c *fiber.Ctx) error {
	q := new(mapping.OrderListQuery)
	if err := c.QueryParser(q); err != nil {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrInvalidBody), nil)
	}
	list := []mapping.OrderListItem{
		{OrderID: "O-1", Status: "paid", Amount: 1234.5},
		{OrderID: "O-2", Status: "pending", Amount: 56.0},
	}
	return code.WriteResponse(c, nil, list)
}

// equityCreate 建仓（走 WriteResponse 裸写结构体）。
func equityCreate(c *fiber.Ctx) error {
	r := new(mapping.StockCreateReq)
	if err := c.BodyParser(r); err != nil {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrInvalidBody), nil)
	}
	resp := &mapping.StockCreateResp{OrderID: "S-1", Symbol: r.Params.Symbol, Qty: r.Params.Quantity}
	return code.WriteResponse(c, nil, resp)
}

// ts 时间戳端点（本地定义类型 + WriteResponse 裸写）。
func ts() fiber.Handler {
	return func(c *fiber.Ctx) error {
		type tsResp struct {
			Ts int64 `json:"ts"`
		}
		return code.WriteResponse(c, nil, tsResp{Ts: time.Now().UnixMilli()})
	}
}

// ping 探活（走 Response 信封，返回 {jsonrpc,id,result:{ok}}）。
func ping() fiber.Handler {
	return func(c *fiber.Ctx) error {
		type rsp struct {
			OK bool `json:"ok"`
		}
		return code.Response(c, nil, &rsp{OK: true})
	}
}
