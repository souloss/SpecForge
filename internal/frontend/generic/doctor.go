package generic

import (
	"fmt"

	"github.com/specforge/specforge/internal/frontend"
	"github.com/specforge/specforge/internal/infer"
)

// Diagnose 实现 frontend.Diagnoser：说明仓库将由通用 LLM 前端处理，并报告源码与候选路由文件规模（成本预估）。
func (*Frontend) Diagnose(repoDir string) []frontend.Check {
	prog, err := NewProgram(repoDir)
	if err != nil {
		return []frontend.Check{{Name: "language", Detail: "cannot scan repository: " + err.Error()}}
	}
	cands := candidateFiles(prog)
	out := []frontend.Check{{Name: "language", OK: true, Detail: fmt.Sprintf(
		"no static frontend matched; the generic LLM frontend will handle it (%d source files, %d candidate route files)",
		len(prog.Files()), len(cands))}}
	llm := frontend.Check{Name: "generic-llm", OK: infer.Configured(),
		Detail: "routes and contracts come from the LLM (verified against source text, confidence capped): run gen with --llm"}
	if !llm.OK {
		llm.Detail = "the generic frontend needs an LLM: set ANTHROPIC_AUTH_TOKEN and run gen with --llm"
	}
	return append(out, llm)
}

// Services 实现 frontend.Diagnoser：文本视图不识别服务边界，整仓为一个服务。
func (*Frontend) Services(string) []frontend.Service { return []frontend.Service{} }
