// Package store 数据访问：接口经多层嵌入，真实实现的方法由嵌入的基础结构体提升而来。
package store

import "errflow.local/repo/internal/code"

// Locker 加锁能力。
type Locker interface {
	Lock(key string) error
}

// Store 服务依赖的存储接口（嵌入 Locker）。
type Store interface {
	Locker
}

// base 真实实现的基础结构体。
type base struct{}

// Lock 加锁失败返回业务码。
func (base) Lock(key string) error {
	if key == "" {
		return code.NewError(code.ErrLocked, "locked")
	}
	return nil
}

// Impl Store 的真实实现（Lock 由 base 提升）。
type Impl struct{ base }
