// Package loader 负责仓库摄入与服务拓扑发现（设计文档 §6.1 步骤 0–1、F1/F2）。
//
// 职责:
//   - 扫描 go.mod 定位模块
//   - 通过 cmd/ 目录与 main 函数定位服务入口
//   - 用 golang.org/x/tools/go/packages 加载带类型信息的全部包
package loader

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Service 描述一个可独立编译/运行的服务单元。
type Service struct {
	Name       string // 服务名，如 "service-ipo"
	Dir        string // 相对仓库根的目录
	Entrypoint string // main 函数所在包
}

// Loaded 仓库加载结果：包集合 + 服务清单。
type Loaded struct {
	Root     string // 绝对路径
	Module   string // module path
	Services []Service
	Pkgs     []*packages.Package
	Fset     *token.FileSet
}

// LoadRepo 加载整个仓库。dir 必须包含 go.mod。
func LoadRepo(dir string) (*Loaded, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(abs, "go.mod")); err != nil {
		return nil, fmt.Errorf("no go.mod under %s: %w", abs, err)
	}
	// 不开 NeedDeps：分析只需仓库内包的语法与类型信息，依赖包走编译器导出数据即可。
	// 开 NeedDeps 会把全部三方依赖从源码解析+类型检查（实测内存 ×6、耗时 ×2~5），产物不变。
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedTypesSizes |
			packages.NeedImports | packages.NeedModule,
		Dir:   abs,
		Env:   os.Environ(),
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}
	var errs []string
	for _, p := range pkgs {
		if len(p.Errors) > 0 && p.Name != "" {
			for _, e := range p.Errors {
				errs = append(errs, fmt.Sprintf("%s: %v", p.PkgPath, e))
			}
		}
	}
	// 容错：只要有类型信息就继续（设计文档 §10.1 tree-sitter 容错解析的对偶）。
	// 完全无法加载时才报错。
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages loaded: %v", strings.Join(errs, "; "))
	}

	l := &Loaded{Root: abs, Pkgs: pkgs, Fset: pkgs[0].Fset}
	for _, p := range pkgs {
		if p.Module != nil && p.Module.Path != "" {
			l.Module = p.Module.Path
			break
		}
	}
	l.Services = detectServices(pkgs)
	return l, nil
}

// detectServices 定位 main 包 → 服务拓扑（F1）。
// 约定: cmd/<name>/ 或 cmd-<name>/ 或根 main 均视为服务。
func detectServices(pkgs []*packages.Package) []Service {
	var out []Service
	for _, p := range pkgs {
		if p.Name != "main" {
			continue
		}
		name := p.PkgPath
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		name = strings.TrimPrefix(strings.TrimPrefix(name, "cmd/"), "cmd-")
		if name == "" || name == "main" {
			name = p.PkgPath
		}
		out = append(out, Service{Name: name, Dir: p.PkgPath, Entrypoint: p.PkgPath})
	}
	return out
}

// ServiceFilter 按名称过滤服务涉及的包集合。
//
// 单模块多服务时，服务边界 = main 包的 import 可达闭包：从服务的入口包
// （cmd/service-xxx 的 main 包）出发，沿 Imports 边 BFS，收集该服务真正
// 可达的包。name 为空返回全量（全仓模式）。找不到对应入口时回退全量，
// 保证不因服务名拼写错误而崩溃或产出空 spec。
func (l *Loaded) ServiceFilter(name string) []*packages.Package {
	if name == "" {
		return l.Pkgs
	}
	var entry *packages.Package
	for _, s := range l.Services {
		if s.Name == name {
			entry = pkgByPath(l.Pkgs, s.Entrypoint)
			break
		}
	}
	if entry == nil {
		return l.Pkgs
	}
	// BFS 沿 import 边收集可达包（含外部依赖，末尾只保留 l.Pkgs 内实际加载的）。
	reach := map[string]*packages.Package{}
	queue := []*packages.Package{entry}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if p == nil || reach[p.PkgPath] != nil {
			continue
		}
		reach[p.PkgPath] = p
		for _, imp := range p.Imports {
			if imp != nil && reach[imp.PkgPath] == nil {
				queue = append(queue, imp)
			}
		}
	}
	// 只保留 ./... 范围内实际加载的包，保持加载序稳定。
	var out []*packages.Package
	for _, p := range l.Pkgs {
		if reach[p.PkgPath] != nil {
			out = append(out, p)
		}
	}
	return out
}

// pkgByPath 在包集合中按 import path 查找包。
func pkgByPath(pkgs []*packages.Package, path string) *packages.Package {
	for _, p := range pkgs {
		if p.PkgPath == path {
			return p
		}
	}
	return nil
}

// scanSkipDirs 轻量服务扫描跳过的目录（与分析无关的依赖、产物、测试数据）。
var scanSkipDirs = map[string]bool{".git": true, ".specforge": true, "vendor": true, "node_modules": true, "testdata": true}

// ListServices 不做类型加载地扫描仓库内的 main 包（只解析 package 子句），返回按名排序的服务清单。
// 服务名口径与 LoadRepo 一致：main 包目录末段，去掉 "cmd-" 前缀。供 doctor 等快速命令使用。
func ListServices(root string) ([]Service, error) {
	seen := map[string]bool{}
	var out []Service
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && (scanSkipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir := filepath.Dir(path)
		if seen[dir] {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly)
		if perr != nil || f.Name.Name != "main" {
			return nil
		}
		seen[dir] = true
		rel, _ := filepath.Rel(root, dir)
		name := strings.TrimPrefix(filepath.Base(dir), "cmd-")
		out = append(out, Service{Name: name, Dir: filepath.ToSlash(rel)})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}
