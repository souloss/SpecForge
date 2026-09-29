package golang

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/specforge/specforge/internal/adapter"
	"github.com/specforge/specforge/internal/frontend"
	"github.com/specforge/specforge/internal/loader"
	"github.com/specforge/specforge/internal/profile"
)

// Diagnose 实现 frontend.Diagnoser：Go 工具链、go.mod、框架依赖、约定画像。
func (*Frontend) Diagnose(repo string) []frontend.Check {
	var out []frontend.Check
	if v, err := exec.Command("go", "env", "GOVERSION").Output(); err != nil {
		out = append(out, frontend.Check{Name: "go", Detail: "go toolchain not found on PATH (needed to type-check the repository)"})
	} else {
		out = append(out, frontend.Check{Name: "go", OK: true, Detail: strings.TrimSpace(string(v))})
	}
	gomod, err := os.ReadFile(filepath.Join(repo, goModFile))
	if err != nil {
		out = append(out, frontend.Check{Name: "go.mod", Detail: "no go.mod under " + repo + " (pass --repo <module root>)"})
	} else {
		out = append(out, frontend.Check{Name: "go.mod", OK: true, Detail: "found"}, frameworkCheck(gomod))
	}
	pp := filepath.Join(repo, ".specforge", "profile.yaml")
	if _, err := os.Stat(pp); errors.Is(err, fs.ErrNotExist) {
		out = append(out, frontend.Check{Name: "profile", OK: true, Detail: "none (built-in defaults + auto-discovered wrappers)"})
	} else if _, err := profile.Load(pp); err != nil {
		out = append(out, frontend.Check{Name: "profile", Detail: pp + ": " + err.Error()})
	} else {
		out = append(out, frontend.Check{Name: "profile", OK: true, Detail: pp})
	}
	return out
}

// frameworkCheck 从 go.mod 依赖识别受支持的框架（modfile 解析，单行与块状 require 均支持）。
func frameworkCheck(gomod []byte) frontend.Check {
	mf, err := modfile.ParseLax(goModFile, gomod, nil)
	if err != nil {
		return frontend.Check{Name: "framework", Detail: "go.mod does not parse: " + err.Error()}
	}
	deps := make([]string, 0, len(mf.Require))
	for _, r := range mf.Require {
		deps = append(deps, r.Mod.Path)
	}
	if fws := adapter.Detect(deps); len(fws) > 0 {
		return frontend.Check{Name: "framework", OK: true, Detail: frameworkNames(fws)}
	}
	return frontend.Check{Name: "framework", Detail: "no supported framework in go.mod (supported: " + strings.Join(adapter.Supported(), ", ") + ")"}
}

// Services 实现 frontend.Diagnoser：仓库内的 main 包（不做类型加载的快速扫描）。
func (*Frontend) Services(repo string) []frontend.Service {
	svcs, _ := loader.ListServices(repo)
	out := make([]frontend.Service, 0, len(svcs))
	for _, s := range svcs {
		out = append(out, frontend.Service{Name: s.Name, Dir: s.Dir})
	}
	return out
}
