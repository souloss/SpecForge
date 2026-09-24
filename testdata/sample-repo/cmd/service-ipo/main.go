// service-ipo 入口（服务拓扑发现的目标）。
package main

import (
	"log"

	"trade.local/repo/internal/ipoServer"
	ipocontroller "trade.local/repo/internal/ipoServer/controller/ipoServer"
	iposervice "trade.local/repo/internal/ipoServer/service/ipoServer"

	"github.com/gofiber/fiber/v2"
)

func main() {
	app := fiber.New()
	svc := iposervice.NewIpoService()
	ctl := ipocontroller.NewIpoController(svc)
	hc := &ipoServer.HealthController{Version: "1.0.0"}
	ipoServer.RegisterRoutes(app, ctl, hc)
	log.Fatal(app.Listen(":8080"))
}
