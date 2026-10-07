// Package mapping 请求/响应 DTO（对应 access-control/internal/access-control/mapping）。
//
// 变体点：请求体是**扁平 DTO**（无 params 外壳）——jsonRpcMiddleware 已把 JSON-RPC 外壳剥掉，
// handler 直接 BodyParser 到裸结构体。
package mapping

// RoleListReq 角色列表查询请求（扁平，无 params 外壳）。
type RoleListReq struct {
	Page  int `json:"page"`
	Count int `json:"count"`
}

// RoleItem 角色条目。
type RoleItem struct {
	RoleID   string `json:"roleId"`
	RoleName string `json:"roleName"`
}

// RoleListResp 角色列表响应业务体。
type RoleListResp struct {
	List  []RoleItem `json:"list"`
	Total int        `json:"total"`
}

// UserCreateReq 用户创建请求（扁平）。
type UserCreateReq struct {
	Uin      string `json:"uin"`
	Name     string `json:"name"`
	RoleCode string `json:"roleCode"`
}

// UserCreateResp 用户创建响应业务体。
type UserCreateResp struct {
	UserID string `json:"userId"`
	Uin    string `json:"uin"`
}

// RoleAssignReq 角色分配请求（扁平）。
type RoleAssignReq struct {
	Uin      string `json:"uin"`
	RoleCode string `json:"roleCode"`
}

// RoleAssignResp 角色分配响应业务体。
type RoleAssignResp struct {
	Assigned bool `json:"assigned"`
}
