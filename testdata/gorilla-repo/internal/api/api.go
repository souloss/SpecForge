// Package api gorilla/mux 路由注册（r.HandleFunc(...).Methods("GET") + mux.Vars(r)["id"]）。
package api

import (
	"encoding/json"
	"net/http"

	"github.com/gorilla/mux"
)

// CreateItemReq 创建条目请求体。
type CreateItemReq struct {
	Name  string  `json:"name"`
	Price float64 `json:"price"`
}

// Item 条目（查询/创建的业务体）。
type Item struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Price float64 `json:"price"`
}

// Register 注册路由（gorilla/mux 的 HandleFunc + Methods 链式风格）。
func Register(r *mux.Router) {
	r.HandleFunc("/items", CreateItem).Methods("POST")
	r.HandleFunc("/items", ListItems).Methods("GET")
	r.HandleFunc("/items/{id}", GetItem).Methods("GET")
	r.HandleFunc("/items/{id}", DeleteItem).Methods("DELETE")
}

// CreateItem 创建条目（成功 201 + Item）。
func CreateItem(w http.ResponseWriter, r *http.Request) {
	var req CreateItemReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(Item{ID: "i-1", Name: req.Name, Price: req.Price})
}

// ListItems 列出条目（成功 200 + 数组）。
func ListItems(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode([]Item{{ID: "i-1", Name: "a", Price: 1.5}})
}

// GetItem 查询条目（成功 200 + Item，404 分支）。
func GetItem(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "missing" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(Item{ID: id, Name: "a", Price: 1.5})
}

// DeleteItem 删除条目（成功 204 无 body）。
func DeleteItem(w http.ResponseWriter, r *http.Request) {
	_ = mux.Vars(r)["id"]
	w.WriteHeader(http.StatusNoContent)
}
