// Command api 启动 echo 样本服务。
package main

import (
	"github.com/labstack/echo/v4"

	"example.com/echorepo/internal/api"
)

func main() {
	e := echo.New()
	api.Register(e)
	_ = e.Start(":8080")
}
