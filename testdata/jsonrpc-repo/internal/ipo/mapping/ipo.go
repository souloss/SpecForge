// Package mapping JSON-RPC 请求/响应 DTO（对应 sample-ipo/internal/common/mapping + ipoServer/mapping）。
package mapping

// BaseData JSON-RPC 请求/响应的公共外壳字段（jsonrpc/id 必填）。
type BaseData struct {
	JsonRpc string `json:"jsonrpc" validate:"required,eq=2.0"`
	Id      string `json:"id" validate:"required"`
}

// BaseRequestParams JSON-RPC 请求体通用外壳：params 承载业务参数（any 由 value-level 收窄）。
type BaseRequestParams struct {
	BaseData
	Params interface{} `json:"params"`
}

// OrderCheckReq 下单合规检查请求（params 外壳）。
type OrderCheckReq struct {
	BaseData
	Params *OrderCheckParams `json:"params" validate:"required"`
}

// OrderCheckParams 合规检查业务参数。
type OrderCheckParams struct {
	OrderID   string  `json:"orderId" validate:"required"`
	AccountID int64   `json:"accountId" validate:"required,min=1"`
	Amount    float64 `json:"amount" validate:"required,min=0.01"`
}

// OrderCheckResp 合规检查响应业务体。
type OrderCheckResp struct {
	OrderID string    `json:"orderId"`
	Status  string    `json:"status"`
	Amount  float64   `json:"amount"`
	Fee     *float64  `json:"fee,omitempty"`
}

// OrderCancelReq 撤单请求（params 外壳）。
type OrderCancelReq struct {
	BaseData
	Params *OrderCancelParams `json:"params" validate:"required"`
}

// OrderCancelParams 撤单业务参数。
type OrderCancelParams struct {
	OrderID string `json:"orderId" validate:"required"`
}

// OrderCancelResp 撤单响应业务体。
type OrderCancelResp struct {
	OrderID   string `json:"orderId"`
	Canceled  bool   `json:"canceled"`
	Timestamp int64  `json:"timestamp"`
}

// OrderListQuery 订单列表查询参数（GET，query 平铺，无 params 外壳）。
type OrderListQuery struct {
	Uin     int `json:"uin" query:"uin" validate:"required"`
	Page    int `json:"page" query:"page" validate:"number,min=1"`
	Count   int `json:"count" query:"count" validate:"number,min=1,max=100"`
}

// OrderListItem 订单列表项。
type OrderListItem struct {
	OrderID string `json:"orderId"`
	Status  string `json:"status"`
	Amount  float64 `json:"amount"`
}

// StockCreateReq 建仓请求（params 外壳，仅用于裸写响应路径）。
type StockCreateReq struct {
	BaseData
	Params *StockCreateParams `json:"params" validate:"required"`
}

// StockCreateParams 建仓业务参数。
type StockCreateParams struct {
	Symbol   string `json:"symbol" validate:"required"`
	Quantity int64  `json:"quantity" validate:"required,min=1"`
}

// StockCreateResp 建仓响应业务体。
type StockCreateResp struct {
	OrderID string `json:"orderId"`
	Symbol  string `json:"symbol"`
	Qty     int64  `json:"qty"`
}
