// Package main 是 SpecForge CLI 入口。
//
// 用法:
//
//	specforge gen  --repo <dir> --service <name> [--profile <file>] [--out <dir>]
//	specforge eval --truth <file> --spec <file> [--json]
//	specforge version
//
// gen: 静态流水线（loader → codegraph → adapter 路由抽取 → typeschema 合成
// → 响应汇聚点追踪 → 事实化 → 确定性编译），产出 openapi.yaml 与置信度报告。
// eval: 将生成 spec 与 ground truth 对比，输出召回 / F1 / 幻觉率等指标。
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/specforge/specforge/internal/compiler"
	"github.com/specforge/specforge/internal/engine"
	"github.com/specforge/specforge/internal/eval"
)

const Version = "0.4.0" // P0+P1 实现: 静态全流水线 + IR + 确定性编译 + 评测

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gen":
		err = cmdGen(os.Args[2:])
	case "eval":
		err = cmdEval(os.Args[2:])
	case "version":
		fmt.Printf("specforge %s (go1.27)\n", Version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `specforge — agent-native OpenAPI generator (P0/P1)

commands:
  gen    generate OpenAPI 3.1 from a Go repo (zero annotations)
  eval   score a generated spec against ground truth
  version
`)
}

type genFlags struct {
	repo    string
	service string
	profile string
	outDir  string
	jsonOut bool
}

func cmdGen(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	repo := fs.String("repo", ".", "target repo root")
	service := fs.String("service", "", "service name filter (empty = all services)")
	profile := fs.String("profile", "", "convention profile yaml (optional)")
	outDir := fs.String("out", "", "output dir (default: <repo>/.specforge/out)")
	jsonOut := fs.Bool("json", false, "agent-friendly JSON summary on stdout")
	fs.Parse(args)

	t0 := time.Now()
	result, err := engine.Run(engine.Config{
		RepoDir:     *repo,
		Service:     *service,
		ProfilePath: *profile,
		OutDir:      *outDir,
	})
	if err != nil {
		return err
	}
	if *outDir == "" {
		*outDir = result.OutDir
	}
	el := time.Since(t0).Round(time.Millisecond)

	specPath := *outDir + "/openapi.yaml"
	reportPath := *outDir + "/report.md"
	fmt.Printf(`specforge gen %s
  services:            %s
  packages:            %d   symbols: %d   call edges: %d
  routes:              %d   (resolved: %d, unresolved-wildcard: %d)
  operations:          %d
  schema types:        %d
  sink sites traced:   %d
  facts:               %d   (dropped by evidence check: %d)
  low-confidence (<0.8): %d   see %s
  output:              %s
  elapsed:             %s
`, Version, result.Services, result.Packages, result.Symbols, result.CallEdges,
		result.Routes, result.RoutesResolved, result.RoutesUnresolved,
		result.Operations, result.SchemaTypes, result.SinkSites,
		result.Facts, result.Dropped, result.LowConf, reportPath, specPath, el)

	// 自确定性检查: 编译两次并比对逐字节 (设计文档 §8.5)
	if !compiler.CompileTwiceCheck(result.Doc) {
		return fmt.Errorf("determinism check FAILED: two compiles produced different bytes")
	}
	fmt.Printf("  determinism:          OK (byte-identical on double compile)\n")

	if *jsonOut {
		return engine.WriteJSONSummary(result, os.Stdout)
	}
	return nil
}

func cmdEval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	truth := fs.String("truth", "", "ground truth openapi yaml")
	spec := fs.String("spec", "", "generated openapi yaml")
	jsonOut := fs.Bool("json", false, "machine-readable output")
	fs.Parse(args)
	if *truth == "" || *spec == "" {
		return fmt.Errorf("--truth and --spec are required")
	}
	m, err := eval.Evaluate(*truth, *spec)
	if err != nil {
		return err
	}
	if *jsonOut {
		return eval.WriteJSON(m, os.Stdout)
	}
	return eval.WriteReport(m, os.Stdout)
}
