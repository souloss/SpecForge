// Package errgroup 与 golang.org/x/sync/errgroup 同构的最小实现（夹具不引外部依赖）。
package errgroup

// Group 任务组。
type Group struct{ err error }

// Go 执行任务。
func (g *Group) Go(f func() error) {
	if err := f(); err != nil && g.err == nil {
		g.err = err
	}
}

// Wait 返回首个错误。
func (g *Group) Wait() error { return g.err }
