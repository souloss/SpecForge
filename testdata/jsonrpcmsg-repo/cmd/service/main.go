// service-perm 入口。
package main

import (
	"log"

	"trade.local/jsonrpcmsg/internal/perm"

	"github.com/gofiber/fiber/v2"
)

func main() {
	app := fiber.New()
	perm.RegisterRoutes(app)
	log.Fatal(app.Listen(":8080"))
}
