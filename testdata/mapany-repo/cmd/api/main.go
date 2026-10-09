// api 服务入口。
package main

import (
	"log"

	"mapany.local/repo/internal/api"

	"github.com/gofiber/fiber/v2"
)

// main 启动 fiber 服务。
func main() {
	app := fiber.New()
	api.RegisterRoutes(app)
	log.Fatal(app.Listen(":8080"))
}
