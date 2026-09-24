// Package ipoServer IPO 控制器层（对应 trade/internal/ipoServer/controller/ipoServer）。
package ipoServer

import (
	"trade.local/repo/internal/ipoServer/mapping"
	iposervice "trade.local/repo/internal/ipoServer/service/ipoServer"
	"trade.local/repo/internal/pkg/code"

	"github.com/gofiber/fiber/v2"
)

// IpoController IPO 控制器。
type IpoController struct {
	srv *iposervice.IpoService
}

// NewIpoController 构造。
func NewIpoController(s *iposervice.IpoService) *IpoController { return &IpoController{srv: s} }

// OrderCheck 订单合规检查。
func (u *IpoController) OrderCheck(c *fiber.Ctx) error {
	r := new(mapping.OrderCheckReqParams)
	if err := c.BodyParser(r); err != nil {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrInvalidBody), nil)
	}
	if err := r.Validate(); err != nil {
		return code.WriteResponse(c, err, nil)
	}
	return u.srv.OrderCheck(c, &r.Params)
}

// OrderCreate 创建订单。
func (u *IpoController) OrderCreate(c *fiber.Ctx) error {
	r := new(mapping.OrderCreateReq)
	if err := c.BodyParser(r); err != nil {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrInvalidBody), nil)
	}
	return u.srv.OrderCreate(c, r)
}

// OrderQuery 查询订单列表。
func (u *IpoController) OrderQuery(c *fiber.Ctx) error {
	r := new(mapping.OrderListReq)
	if err := c.BodyParser(r); err != nil {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrInvalidBody), nil)
	}
	return u.srv.OrderQuery(c, r)
}

// GetOrderDetail 查询订单详情。
func (u *IpoController) GetOrderDetail(c *fiber.Ctx) error {
	orderID := c.Params("orderId")
	if orderID == "" {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrValidateFail), nil)
	}
	return u.srv.GetOrderDetail(c, orderID)
}

// ListOrderItems 查询订单成交明细。
func (u *IpoController) ListOrderItems(c *fiber.Ctx) error {
	orderID := c.Params("orderId")
	if orderID == "" {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrValidateFail), nil)
	}
	return u.srv.ListOrderItems(c, orderID)
}

// OrderCancel 撤销订单。
func (u *IpoController) OrderCancel(c *fiber.Ctx) error {
	r := new(mapping.OrderCheckReqParams)
	if err := c.BodyParser(r); err != nil {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrInvalidBody), nil)
	}
	return u.srv.OrderCancel(c, &r.Params)
}

// RiskAssess 风险评估。
func (u *IpoController) RiskAssess(c *fiber.Ctx) error {
	orderID := c.Query("orderId")
	if orderID == "" {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrValidateFail), nil)
	}
	level := c.Query("level")
	_ = level
	return u.srv.RiskAssess(c, orderID)
}

// LegacyQuote 旧版行情（表驱动注册的 handler）。
func (u *IpoController) LegacyQuote(c *fiber.Ctx) error {
	symbol := c.Query("symbol")
	if symbol == "" {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrValidateFail), nil)
	}
	return u.srv.LegacyQuote(c, symbol)
}

// LegacyDepth 旧版深度（表驱动注册的 handler）。
func (u *IpoController) LegacyDepth(c *fiber.Ctx) error {
	symbol := c.Query("symbol")
	return u.srv.LegacyDepth(c, symbol)
}
