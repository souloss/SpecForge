// Package svc 泛型分页 List 的 any 缺口的用例。
//
// 契约点：Page[T] 泛型的 List 是 []T，静态值流追不到类型实参 T（真实仓库同为
// utils.Page[T any] 形态），List 落成 interface{}/any 缺口。见 PATTERNS.md §9。
package svc

import (
	"genericpage.local/repo/internal/pkg/code"

	"github.com/gofiber/fiber/v2"
)

// Item 列表元素。
type Item struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Order 另一列表元素类型。
type Order struct {
	OrderID string `json:"orderId"`
	Amount  int64  `json:"amount"`
}

// Page 泛型分页：List 元素类型由 T 实例化决定。
type Page[T any] struct {
	Total int64 `json:"total"`
	List  []T   `json:"list"`
}

// NewPage 构造分页。
func NewPage[T any](items []T, total int64) Page[T] { return Page[T]{Total: total, List: items} }

// RegisterRoutes 注册路由。
func RegisterRoutes(app *fiber.App) {
	app.Get("/items", listItems)
	app.Get("/orders", listOrders)
	app.Get("/accounts", listAccounts)
}

// listItems 返回 Page[Item]。
func listItems(c *fiber.Ctx) error {
	items := []Item{{ID: 1, Name: "a"}}
	return code.Response(c, nil, NewPage(items, 1))
}

// listOrders 返回 Page[Order]。
func listOrders(c *fiber.Ctx) error {
	orders := []Order{{OrderID: "o1", Amount: 9}}
	return code.Response(c, nil, NewPage(orders, 1))
}

// listAccounts 返回 Page[string]。
func listAccounts(c *fiber.Ctx) error {
	accts := []string{"a1", "a2"}
	return code.Response(c, nil, NewPage(accts, 2))
}
