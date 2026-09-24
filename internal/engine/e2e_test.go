package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/specforge/specforge/internal/compiler"
	"github.com/specforge/specforge/internal/eval"
)

// TestEndToEndAccuracy 端到端精度回归守卫:
// 生成 → 评测 → 全指标必须维持满分基线（设计文档 §1.6 验收标准）。
// 任何使指标回退的改动都会在此测试失败——这是「文档进 CI gate」
// 的前提（设计文档 §8.5 确定性自检的对偶）。
func TestEndToEndAccuracy(t *testing.T) {
	repo := filepath.Join("..", "..", "testdata", "sample-repo")
	outDir := t.TempDir()

	result, err := Run(Config{
		RepoDir:     repo,
		ProfilePath: filepath.Join(repo, ".specforge", "profile.yaml"),
		OutDir:      outDir,
	})
	if err != nil {
		t.Fatalf("gen failed: %v", err)
	}
	if result.Operations != 10 {
		t.Errorf("operations = %d, want 10", result.Operations)
	}
	if result.Routes != 10 || result.RoutesResolved != 10 {
		t.Errorf("routes = %d (resolved %d), want 10/10", result.Routes, result.RoutesResolved)
	}
	// 确定性: 双编译逐字节一致
	if !compileTwice(result) {
		t.Error("determinism check failed")
	}

	// 评测: 全指标满分基线
	truth := filepath.Join("..", "..", "testdata", "ground-truth.yaml")
	spec := filepath.Join(outDir, "openapi.yaml")
	m, err := eval.Evaluate(truth, spec)
	if err != nil {
		t.Fatalf("eval failed: %v", err)
	}
	checks := []struct {
		name string
		got  float64
		want float64
	}{
		{"RouteRecall", m.RouteRecall, 1.0},
		{"RoutePrecision", m.RoutePrecision, 1.0},
		{"ParamF1", m.ParamF1, 1.0},
		{"ReqFieldF1", m.ReqFieldF1, 1.0},
		{"RespFieldF1", m.RespFieldF1, 1.0},
		{"EnvelopeRecall", m.EnvelopeRecall, 1.0},
	}
	for _, c := range checks {
		if c.got < c.want {
			t.Errorf("%s = %.3f, want >= %.3f", c.name, c.got, c.want)
		}
	}
	if m.HallucRate > 0 {
		t.Errorf("Hallucination = %.3f, want 0", m.HallucRate)
	}

	// 输出文件存在性
	if _, err := os.Stat(spec); err != nil {
		t.Error("openapi.yaml missing")
	}
	if _, err := os.Stat(filepath.Join(outDir, "report.md")); err != nil {
		t.Error("report.md missing")
	}
}

// compileTwice 引擎侧确定性检查（设计文档 §8.5 双跑自检）。
func compileTwice(r *Result) bool {
	if r.Doc == nil {
		return false
	}
	return compiler.CompileTwiceCheck(r.Doc)
}
