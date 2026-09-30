// Package mocks 测试替身：不应进入值流。
package mocks

import "errors"

// Store mock。
type Store struct{}

// Lock mock 实现。
func (Store) Lock(string) error { return errors.New("mock") }
