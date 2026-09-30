package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/specforge/specforge/internal/compiler"
)

// errflowExpect 夹具中一个 operation 的错误行定性期望。
type errflowExpect struct {
	codes      []int // 常量业务码
	dynamic    bool  // 存在动态码行
	uncoded    bool  // 存在不带业务码行
	unresolved bool  // 存在未解析行（LLM 兜底目标）
}

// TestErrorFlowClassification 错误值流的静态定性回归：仓库外构造的错误 → 不带业务码，
// 运行时字段取码 → 动态码，errgroup 汇聚任务错误，嵌入接口提升的方法解析到真实实现（mock 与转发型排除），
// 恒 nil 的错误只有成功行；以上均不应留下未解析行。
func TestErrorFlowClassification(t *testing.T) {
	outDir := t.TempDir()
	if _, err := Run(context.Background(), Config{RepoDir: filepath.Join("..", "..", "testdata", "errflow-repo"), OutDir: outDir}); err != nil {
		t.Fatalf("gen failed: %v", err)
	}
	f, err := ReadOperations(outDir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]errflowExpect{
		"/uncoded":  {codes: []int{100404}, uncoded: true},
		"/dynamic":  {codes: []int{0}, dynamic: true},
		"/group":    {codes: []int{100404}, uncoded: true},
		"/embedded": {codes: []int{100423}},
		"/static":   {codes: []int{0}},
	}
	for _, op := range f.Operations {
		w, ok := want[op.Path]
		if !ok {
			continue
		}
		delete(want, op.Path)
		if got := summarize(op); !equalExpect(got, w) {
			t.Errorf("%s: got %+v, want %+v", op.Path, got, w)
		}
	}
	for p := range want {
		t.Errorf("operation %s missing", p)
	}
}

// summarize 把 operation 的响应行归并为错误行定性摘要。
func summarize(op compiler.Operation) errflowExpect {
	var e errflowExpect
	for _, r := range op.Responses {
		e.codes = append(e.codes, r.Codes...)
		e.dynamic = e.dynamic || r.HasDynamic
		e.uncoded = e.uncoded || r.HasUncoded
		e.unresolved = e.unresolved || r.HasUnresolved
	}
	return e
}

// equalExpect 两个定性摘要是否一致（码按出现顺序比较）。
func equalExpect(a, b errflowExpect) bool {
	if len(a.codes) != len(b.codes) || a.dynamic != b.dynamic || a.uncoded != b.uncoded || a.unresolved != b.unresolved {
		return false
	}
	for i := range a.codes {
		if a.codes[i] != b.codes[i] {
			return false
		}
	}
	return true
}
