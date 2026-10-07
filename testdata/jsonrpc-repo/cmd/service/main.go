// service-ipo 入口（服务拓扑发现目标）。
package main

import (
	"log"

	"trade.local/jsonrpc/internal/ipo"

	"github.com/gofiber/fiber/v2"
)

func main() {
	app := fiber.New()
	ipo.RegisterRoutes(app)
	log.Fatal(app.Listen(":8080"))
}
