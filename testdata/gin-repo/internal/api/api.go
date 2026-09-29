// Package api 是 gin 适配器的样本服务：分组、路径参数、query/header 读取、JSON/Query/URI 绑定、
// 带状态码的写出与响应包装器。
package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// CreateUserReq 创建用户请求体。
type CreateUserReq struct {
	Name  string `json:"name" binding:"required"`
	Email string `json:"email"`
}

// User 用户。
type User struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

// ListQuery 列表查询参数。
type ListQuery struct {
	Page int    `form:"page"`
	Sort string `form:"sort"`
}

// ErrBody 错误响应体。
type ErrBody struct {
	Message string `json:"message"`
}

// Register 注册路由。
func Register(r *gin.Engine) {
	v1 := r.Group("/api/v1")
	users := v1.Group("/users")
	users.POST("", CreateUser)
	users.GET("/:id", GetUser)
	users.GET("", ListUsers)
	users.GET("/:id/detail", Detail)
	v1.GET("/stats", Stats)
	users.GET("/:id/settings", Settings)
	users.GET("/:id/profile", Profile)
}

// CreateUser 创建用户。
func CreateUser(c *gin.Context) {
	var req CreateUserReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrBody{Message: err.Error()})
		return
	}
	c.JSON(http.StatusCreated, User{ID: 1, Name: req.Name, Email: req.Email})
}

// GetUser 查询单个用户。
func GetUser(c *gin.Context) {
	_ = c.Param("id")
	_ = c.GetHeader("X-Trace-Id")
	ok(c, User{ID: 1})
}

// ListUsers 分页查询用户。
func ListUsers(c *gin.Context) {
	var q ListQuery
	_ = c.ShouldBindQuery(&q)
	_ = c.Query("keyword")
	c.JSON(http.StatusOK, []User{})
}

// ok 响应包装器：直接写出业务体。
func ok(c *gin.Context, data any) {
	c.JSON(http.StatusOK, data)
}

// errNotFound 查询不到用户的业务错误。
var errNotFound = errors.New("user not found")

// Resp 响应包装器：gin.H map 字面量信封，失败分支固定业务码 500。
func Resp(c *gin.Context, err error, data any) {
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"code": 500, "msg": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "msg": "ok", "data": data})
}

// Detail 用户详情（经 gin.H 信封包装器写出）。
func Detail(c *gin.Context) {
	if c.Param("id") == "" {
		Resp(c, errNotFound, nil)
		return
	}
	Resp(c, nil, User{ID: 1})
}

// Stats 统计（handler 内直接写出，成功与失败分支的 body 形状不同）。
func Stats(c *gin.Context) {
	if err := c.ShouldBindQuery(&ListQuery{}); err != nil {
		c.JSON(http.StatusOK, gin.H{"error": err.Error()})
		return
	}
	total := 3
	c.JSON(http.StatusOK, gin.H{"total": total, "active": true})
}

// RespV2 包装器：先建 map 再逐键赋值的信封写法。
func RespV2(c *gin.Context, data any) {
	m := gin.H{"code": 0}
	m["data"] = data
	c.JSON(http.StatusOK, m)
}

// Settings 用户设置（经 RespV2 写出）。
func Settings(c *gin.Context) {
	RespV2(c, ListQuery{Page: 1})
}

// Wrap 包装器：经辅助函数构造信封（静态摘要识别不出，留给 LLM 包装器摘要兜底）。
func Wrap(c *gin.Context, data any) {
	c.JSON(http.StatusOK, envelope(data))
}

// envelope 构造 {ok, payload} 信封。
func envelope(d any) gin.H {
	return gin.H{"ok": true, "payload": d}
}

// Profile 用户资料（经 Wrap 写出）。
func Profile(c *gin.Context) {
	Wrap(c, User{ID: 2})
}
