// Package api 是标准库 net/http 样本服务的路由与处理器实现。
//
// 采用 Go 1.22+ 的 ServeMux 方法+路径模式注册（mux.HandleFunc("GET /items", ...)），
// 请求体用 json.Decoder 解码、路径参数用 r.PathValue("id") 读取、响应用 json.Encoder 写出。
// 这是一个「未内置适配器」的形态：引擎当前对 net/http ServeMux 无路由抽取适配器。
package api

import (
	"encoding/json"
	"net/http"
)

// CreateItemReq 创建条目请求体。
type CreateItemReq struct {
	Name  string  `json:"name"`
	Price float64 `json:"price"`
	Qty   int     `json:"qty"`
}

// Item 条目响应结构体。
type Item struct {
	ID    int64   `json:"id"`
	Name  string  `json:"name"`
	Price float64 `json:"price"`
	Qty   int     `json:"qty"`
}

// Register 在给定 ServeMux 上注册全部路由。
func Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /items", ListItems)
	mux.HandleFunc("POST /items", CreateItem)
	mux.HandleFunc("GET /items/{id}", GetItem)
	mux.HandleFunc("DELETE /items/{id}", DeleteItem)
}

// ListItems 返回全部条目列表（裸写 []Item 结构体数组，无信封，真实状态码 200）。
func ListItems(w http.ResponseWriter, r *http.Request) {
	_ = json.NewEncoder(w).Encode([]Item{{ID: 1, Name: "alpha", Price: 9.9, Qty: 3}})
}

// CreateItem 解码请求体并返回新建条目（201）。
func CreateItem(w http.ResponseWriter, r *http.Request) {
	var req CreateItemReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(Item{ID: 2, Name: req.Name, Price: req.Price, Qty: req.Qty})
}

// GetItem 按路径参数 id 返回单个条目（200；未找到返回 404）。
func GetItem(w http.ResponseWriter, r *http.Request) {
	_ = r.PathValue("id")
	http.Error(w, "not found", http.StatusNotFound)
}

// DeleteItem 按路径参数 id 删除条目（204 无响应体）。
func DeleteItem(w http.ResponseWriter, r *http.Request) {
	_ = r.PathValue("id")
	w.WriteHeader(http.StatusNoContent)
}
