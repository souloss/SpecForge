// Package main 服务入口。
package main

import (
	"headerconst.local/repo/internal/svc"

	"github.com/gofiber/fiber/v2"
)

func main() {
	app := fiber.New()
	svc.RegisterRoutes(app)
}
