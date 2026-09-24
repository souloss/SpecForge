// Package ipoServer IPO 服务层（响应契约的真源：WriteResponse 在这里被调用）。
package ipoServer

import (
	"time"

	"trade.local/repo/internal/ipoServer/mapping"
	"trade.local/repo/internal/pkg/code"

	"github.com/gofiber/fiber/v2"
)

// MaxOrderAmount 单笔限额。
const MaxOrderAmount = 1000000.0

// IpoService IPO 业务服务。
type IpoService struct{}

// NewIpoService 构造。
func NewIpoService() *IpoService { return &IpoService{} }

// OrderCheck 合规检查（深层响应: contract 在 service 层写出）。
func (s *IpoService) OrderCheck(c *fiber.Ctx, p *mapping.OrderCheckParams) error {
	if p.Amount > MaxOrderAmount {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrAmountExceed), nil)
	}
	if p.OrderID == "" {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrValidateFail), nil)
	}
	fee := p.Amount * 0.001
	resp := &mapping.OrderCheckResp{
		OrderID:   p.OrderID,
		Status:    mapping.StatusPending,
		Amount:    p.Amount,
		Fee:       &fee,
		CreatedAt: time.Now(),
	}
	return code.WriteResponse(c, nil, resp)
}

// OrderCreate 创建订单。
func (s *IpoService) OrderCreate(c *fiber.Ctx, r *mapping.OrderCreateReq) error {
	existing := findBySymbol(r.Symbol)
	if existing != "" {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrOrderExists), nil)
	}
	resp := &mapping.OrderCreateResp{
		OrderID:   newOrderID(r.Symbol),
		Status:    mapping.StatusPending,
		CreatedAt: time.Now(),
	}
	return code.WriteResponse(c, nil, resp)
}

// OrderQuery 查询订单列表（切片数据响应）。
func (s *IpoService) OrderQuery(c *fiber.Ctx, r *mapping.OrderListReq) error {
	list := []mapping.OrderListResp{
		{
			OrderID:   "O-1",
			Status:    mapping.StatusPaid,
			Amount:    1234.5,
			CreatedAt: time.Now(),
		},
	}
	return code.WriteResponse(c, nil, list)
}

// GetOrderDetail 订单详情（嵌套 + 可选扩展 + RawMessage）。
func (s *IpoService) GetOrderDetail(c *fiber.Ctx, orderID string) error {
	detail := &mapping.OrderDetailResp{
		OrderID: orderID,
		Status:  mapping.StatusPaid,
		Items: []mapping.OrderItem{
			{ItemID: "I-1", Qty: 100, Price: 12.5},
		},
		Extra: &mapping.OrderExtra{
			Remark: "snapshot",
			Attrs:  map[string]string{"src": "ipo"},
		},
	}
	return code.WriteResponse(c, nil, detail)
}

// ListOrderItems 成交明细列表。
func (s *IpoService) ListOrderItems(c *fiber.Ctx, orderID string) error {
	items := []mapping.OrderItem{
		{ItemID: "I-1", Qty: 100, Price: 12.5},
		{ItemID: "I-2", Qty: 50, Price: 12.6},
	}
	return code.WriteResponse(c, nil, items)
}

// OrderCancel 撤单。
func (s *IpoService) OrderCancel(c *fiber.Ctx, p *mapping.OrderCheckParams) error {
	if p.OrderID == "closed" {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrOrderClosed), nil)
	}
	resp := &mapping.OrderCancelResp{
		OrderID:   p.OrderID,
		Canceled:  true,
		Timestamp: time.Now(),
	}
	return code.WriteResponse(c, nil, resp)
}

// RiskAssess 风险评估（any 字段: 静态不可定型，显式降级）。
func (s *IpoService) RiskAssess(c *fiber.Ctx, orderID string) error {
	resp := &mapping.RiskAssessResp{
		OrderID:  orderID,
		Score:    85,
		RiskInfo: map[string]any{"level": "mid", "tags": []string{"a", "b"}},
	}
	return code.WriteResponse(c, nil, resp)
}

// LegacyQuote 旧版行情。
func (s *IpoService) LegacyQuote(c *fiber.Ctx, symbol string) error {
	type quoteResp struct {
		Symbol string  `json:"symbol"`
		Price  float64 `json:"price"`
	}
	return code.WriteResponse(c, nil, &quoteResp{Symbol: symbol, Price: 1.0})
}

// LegacyDepth 旧版深度（本地定义类型响应）。
func (s *IpoService) LegacyDepth(c *fiber.Ctx, symbol string) error {
	type depthResp struct {
		Symbol   string  `json:"symbol"`
		BidPrice float64 `json:"bidPrice"`
	}
	return code.WriteResponse(c, nil, &depthResp{Symbol: symbol})
}

func findBySymbol(symbol string) string { return "" }

func newOrderID(symbol string) string { return "O-" + symbol }
