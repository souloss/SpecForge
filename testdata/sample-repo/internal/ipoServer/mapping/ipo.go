// Package mapping 请求/响应 DTO（对应 trade/internal/ipoServer/mapping）。
package mapping

import (
        "encoding/json"
        "time"

        "trade.local/repo/internal/pkg/code"
)

// OrderCheckReqParams 下单合规检查请求。
type OrderCheckReqParams struct {
        Params OrderCheckParams `json:"params" binding:"required"`
}

// OrderCheckParams 合规检查业务参数。
type OrderCheckParams struct {
        OrderID   string  `json:"orderId" binding:"required,min=1,max=64"`
        AccountID int64   `json:"accountId" binding:"required,min=1"`
        Amount    float64 `json:"amount" binding:"required,min=0.01,max=1000000"`
}

// OrderCheckResp 合规检查响应。
type OrderCheckResp struct {
        OrderID   string    `json:"orderId"`
        Status    OrderStatus `json:"status"`
        Amount    float64   `json:"amount"`
        Fee       *float64  `json:"fee,omitempty"`
        CreatedAt time.Time `json:"createdAt"`
}

// OrderStatus 订单状态枚举。
type OrderStatus string

// 订单状态常量组（枚举证据）。
const (
        StatusPending  OrderStatus = "pending"
        StatusPaid     OrderStatus = "paid"
        StatusClosed   OrderStatus = "closed"
        StatusCanceled OrderStatus = "canceled"
)

// OrderCreateReq 下单请求。
type OrderCreateReq struct {
        Symbol    string    `json:"symbol" binding:"required,oneof=SH SZ BJ"`
        Price     *float64  `json:"price,omitempty" binding:"omitempty,min=0.01"`
        Quantity  int64     `json:"quantity" binding:"required,min=1,max=1000000"`
        OrderType OrderType `json:"orderType" binding:"required,oneof=limit market"`
}

// OrderType 委托类型枚举。
type OrderType string

// 委托类型常量组。
const (
        OrderTypeLimit  OrderType = "limit"
        OrderTypeMarket OrderType = "market"
)

// OrderCreateResp 下单响应。
type OrderCreateResp struct {
        OrderID   string    `json:"orderId"`
        Status    OrderStatus `json:"status"`
        CreatedAt time.Time `json:"createdAt"`
}

// OrderListReq 订单列表查询请求（跨接口共用的分页类型）。
type OrderListReq struct {
        Page      int    `json:"page" binding:"min=1,max=1000"`
        PageSize  int    `json:"pageSize" binding:"min=1,max=100"`
        Status    *OrderStatus `json:"status,omitempty" binding:"omitempty,oneof=pending paid closed canceled"`
}

// OrderListResp 订单列表项。
type OrderListResp struct {
        OrderID   string      `json:"orderId"`
        Status    OrderStatus `json:"status"`
        Amount    float64     `json:"amount"`
        CreatedAt time.Time   `json:"createdAt"`
}

// OrderItem 订单成交明细。
type OrderItem struct {
        ItemID string  `json:"itemId"`
        Qty    int64   `json:"qty"`
        Price  float64 `json:"price"`
}

// OrderDetailResp 订单详情响应（含嵌套明细与可选扩展）。
type OrderDetailResp struct {
        OrderID string      `json:"orderId"`
        Status  OrderStatus `json:"status"`
        Items   []OrderItem `json:"items"`
        Extra   *OrderExtra `json:"extra,omitempty"`
}

// OrderExtra 订单扩展信息。
type OrderExtra struct {
        Remark  string            `json:"remark,omitempty"`
        Attrs   map[string]string `json:"attrs,omitempty"`
        RawMeta json.RawMessage   `json:"rawMeta,omitempty"`
}

// OrderCancelResp 撤单响应。
type OrderCancelResp struct {
        OrderID   string    `json:"orderId"`
        Canceled  bool      `json:"canceled"`
        Timestamp time.Time `json:"timestamp"`
}

// RiskAssessResp 风险评估响应（RiskInfo 类型层为 any；本 operation 由 map 字面量构造，值级收窄可定型）。
type RiskAssessResp struct {
        OrderID string         `json:"orderId"`
        Score   int            `json:"score"`
        RiskInfo map[string]any `json:"riskInfo"`
}

// HealthStatus 健康检查响应。
type HealthStatus struct {
        Status  string `json:"status"`
        Version string `json:"version"`
}

// Validate 手写校验（信封码不可静态解析的 err 变量路径）。
func (r *OrderCheckReqParams) Validate() error {
        if r.Params.OrderID == "" {
                return code.NewDefaultError(code.ErrValidateFail)
        }
        return nil
}

// Validate 订单列表请求校验。
func (r *OrderListReq) Validate() error {
        return nil
}
