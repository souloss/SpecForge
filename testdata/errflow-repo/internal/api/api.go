// Package api handler：覆盖错误值流的各类静态定性。
package api

import (
	"errors"
	"strconv"

	"errflow.local/repo/internal/code"
	"errflow.local/repo/internal/errgroup"
	"errflow.local/repo/internal/store"

	"github.com/gofiber/fiber/v2"
)

// upstream 上游响应（业务码在运行时取得）。
type upstream struct {
	Code int
	Msg  string
}

// API handler 集合；嵌入 Store 接口（转发型「实现」不应被当作实现）。
type API struct {
	store.Store
}

// Uncoded 仓库外构造的错误（stdlib errors / strconv）与常量业务码并存。
func (a *API) Uncoded(c *fiber.Ctx) error {
	if c.Query("id") == "" {
		return code.Response(c, errors.New("missing id"), nil)
	}
	if _, err := strconv.Atoi(c.Query("id")); err != nil {
		return code.Response(c, err, nil)
	}
	return code.Response(c, code.NewError(code.ErrNotFound, "not found"), nil)
}

// Dynamic 业务码取自上游响应字段。
func (a *API) Dynamic(c *fiber.Ctx) error {
	res := call()
	if res.Code != 0 {
		return code.Response(c, code.NewError(res.Code, res.Msg), nil)
	}
	return code.Response(c, nil, res)
}

// call 模拟上游调用。
func call() upstream { return upstream{} }

// Group errgroup 汇聚的任务错误。
func (a *API) Group(c *fiber.Ctx) error {
	var g errgroup.Group
	g.Go(func() error { return code.NewError(code.ErrNotFound, "nf") })
	g.Go(func() error { return errors.New("upstream down") })
	return code.Response(c, g.Wait(), nil)
}

// Embedded 经嵌入接口调用：解析到 base.Lock（提升方法），mock 与转发型不参与。
func (a *API) Embedded(c *fiber.Ctx) error {
	return code.Response(c, a.Lock(c.Query("k")), nil)
}

// Static 被调方只返回 nil 错误：只有成功写出。
func (a *API) Static(c *fiber.Ctx) error {
	data, err := list()
	return code.Response(c, err, data)
}

// list 静态列表（不会失败）。
func list() ([]string, error) { return []string{"a"}, nil }
