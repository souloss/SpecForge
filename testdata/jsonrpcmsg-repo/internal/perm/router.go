// Package perm 权限服务：service 层组装信封，controller 裸写（对应 access-control 的 BuildBaseResponse 模式）。
package perm

import (
	"encoding/json"

	"trade.local/jsonrpcmsg/internal/perm/mapping"
	"trade.local/jsonrpcmsg/internal/pkg/code"

	"github.com/gofiber/fiber/v2"
)

// RegisterRoutes 注册路由（jsonRpcMiddleware 透明剥壳）。
func RegisterRoutes(app *fiber.App) {
	v1 := app.Group("/permAdmin/v1", jsonRpcMiddleware())
	v1.Post("/RoleList", roleList)
	v1.Post("/UserCreate", userCreate)
	v1.Post("/RoleAssign", roleAssign)
}

// jsonRpcMiddleware 请求透明剥壳：存在 params 时替换 body 为 params 内层（对应 access-control/router.go:132-147）。
func jsonRpcMiddleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		var outer map[string]interface{}
		if err := json.Unmarshal(c.Body(), &outer); err != nil {
			return c.Next()
		}
		if params, ok := outer["params"]; ok {
			if raw, err := json.Marshal(params); err == nil {
				c.Request().SetBody(raw)
			}
		}
		return c.Next()
	}
}

// roleList 角色列表：service 组装信封，controller 裸写。
func roleList(c *fiber.Ctx) error {
	r := new(mapping.RoleListReq)
	if err := c.BodyParser(r); err != nil {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrInvalidBody), nil)
	}
	// service 层语义：查角色
	items := []mapping.RoleItem{
		{RoleID: "R-1", RoleName: "admin"},
		{RoleID: "R-2", RoleName: "operator"},
	}
	payload := &mapping.RoleListResp{List: items, Total: len(items)}
	return code.WriteResponse(c, nil, code.BuildBaseResponse(c, payload))
}

// userCreate 创建用户。
func userCreate(c *fiber.Ctx) error {
	r := new(mapping.UserCreateReq)
	if err := c.BodyParser(r); err != nil {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrInvalidBody), nil)
	}
	if r.Uin == "" {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrPermission), nil)
	}
	payload := &mapping.UserCreateResp{UserID: "U-1", Uin: r.Uin}
	return code.WriteResponse(c, nil, code.BuildBaseResponse(c, payload))
}

// roleAssign 分配角色（错误分支走 error.msg 字段）。
func roleAssign(c *fiber.Ctx) error {
	r := new(mapping.RoleAssignReq)
	if err := c.BodyParser(r); err != nil {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrInvalidBody), nil)
	}
	if r.RoleCode == "super" {
		return code.WriteResponse(c, code.NewDefaultError(code.ErrRoleNotFound), nil)
	}
	payload := &mapping.RoleAssignResp{Assigned: true}
	return code.WriteResponse(c, nil, code.BuildBaseResponse(c, payload))
}
