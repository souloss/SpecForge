// Package api 是 echo 样本服务：没有内置适配器，用于验证 L3（LLM 生成适配器声明）。
package api

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// CreateOrderReq 下单请求体。
type CreateOrderReq struct {
	Symbol string  `json:"symbol"`
	Qty    int     `json:"qty"`
	Price  float64 `json:"price"`
}

// Order 订单。
type Order struct {
	ID     int64  `json:"id"`
	Symbol string `json:"symbol"`
	Status string `json:"status"`
}

// Register 注册路由。
func Register(e *echo.Echo) {
	g := e.Group("/api/v1")
	g.POST("/orders", CreateOrder)
	g.GET("/orders/:id", GetOrder)
	g.GET("/orders", ListOrders)
}

// CreateOrder 下单。
func CreateOrder(c echo.Context) error {
	var req CreateOrderReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusCreated, Order{ID: 1, Symbol: req.Symbol, Status: "new"})
}

// GetOrder 查询订单。
func GetOrder(c echo.Context) error {
	_ = c.Param("id")
	return c.JSON(http.StatusOK, Order{ID: 1})
}

// ListOrders 订单列表。
func ListOrders(c echo.Context) error {
	_ = c.QueryParam("status")
	return c.JSON(http.StatusOK, []Order{})
}
