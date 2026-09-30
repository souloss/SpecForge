// api 服务入口。
package main

import (
	"errflow.local/repo/internal/api"
	"errflow.local/repo/internal/store"

	"github.com/gofiber/fiber/v2"
)

func main() {
	app := fiber.New()
	h := &api.API{Store: &store.Impl{}}
	app.Get("/uncoded", h.Uncoded)
	app.Get("/dynamic", h.Dynamic)
	app.Get("/group", h.Group)
	app.Get("/embedded", h.Embedded)
	app.Get("/static", h.Static)
	_ = app.Listen(":8080")
}
