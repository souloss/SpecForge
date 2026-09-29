// Command api 启动 gin 样本服务。
package main

import (
	"github.com/gin-gonic/gin"

	"example.com/ginrepo/internal/api"
)

func main() {
	r := gin.Default()
	api.Register(r)
	_ = r.Run()
}
