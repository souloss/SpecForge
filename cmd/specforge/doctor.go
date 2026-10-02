package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"

	"github.com/specforge/specforge/internal/engine"
	"github.com/specforge/specforge/internal/frontend"
	"github.com/specforge/specforge/internal/infer"
)

// doctorResult doctor 的机器模式输出。
type doctorResult struct {
	OK       bool               `json:"ok"`       // 全部检查通过
	Language string             `json:"language"` // 识别出的语言前端（未识别为空）
	Checks   []frontend.Check   `json:"checks"`   // 检查明细（顺序固定）
	Services []frontend.Service `json:"services"` // 仓库内的服务（--service 可用的名字）
}

// newDoctorCmd doctor：检查运行环境与仓库前提（不调用 LLM、不写产物）。
func newDoctorCmd(a *app) *cobra.Command {
	var repo, cacheDir string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check toolchain, repository, profile and LLM configuration; list services",
		Long: `Run quick pre-flight checks before 'gen' without analyzing the code or calling the LLM:

  language   the repository matches the Go or generic source frontend
  go         a Go toolchain is on PATH (needed by the Go frontend)
  go.mod     --repo points at a module root
  framework  a supported web framework is a dependency (gofiber/v2, gin, chi/v4, chi/v5)
  profile    <repo>/.specforge/profile.yaml parses (absent is fine)
  llm        whether LLM credentials are configured (needed by generic or --llm)
  cache      the cache directory is writable

It also lists the services found in the repository (main packages); these are the names
accepted by 'gen --service'. Exits 30 if any check fails.`,
		Example: `  specforge doctor
  specforge doctor --repo ../sample-ipo --cache-dir /tmp/specforge-cache --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res := doctorResult{OK: true, Checks: runChecks(repo, cacheDir), Services: []frontend.Service{}}
			if fe, err := engine.DetectFrontend(repo); err == nil {
				res.Language = fe.Name()
				if d, ok := fe.(frontend.Diagnoser); ok {
					res.Checks = append(d.Diagnose(repo), res.Checks...)
					res.Services = d.Services(repo)
				}
			} else {
				res.Checks = append([]frontend.Check{{Name: "language", Detail: "no supported project found (supported: " +
					strings.Join(engine.SupportedLanguages(), ", ") + "); pass --repo <project root>"}}, res.Checks...)
			}
			for _, c := range res.Checks {
				res.OK = res.OK && c.OK
			}
			a.emit(res, func(w io.Writer) { printDoctor(w, res) })
			if !res.OK {
				return newErr(exitConfig, codeConfigInvalid, "some checks failed", "fix the FAIL items above")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "repository root")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "cache root (default: <repo>/.specforge/cache)")
	return cmd
}

// printDoctor 人类模式输出。
func printDoctor(w io.Writer, res doctorResult) {
	for _, c := range res.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "[%s] %-10s %s\n", mark, c.Name, c.Detail)
	}
	fmt.Fprintf(w, "services (%d):", len(res.Services))
	for _, s := range res.Services {
		fmt.Fprintf(w, " %s", s.Name)
	}
	fmt.Fprintln(w)
}

// runChecks 语言无关的检查：LLM 凭据与缓存目录（语言相关检查由前端的 Diagnoser 提供）。
func runChecks(repo, cacheDir string) []frontend.Check {
	var out []frontend.Check
	if infer.Configured() {
		base := os.Getenv("ANTHROPIC_BASE_URL")
		if base == "" {
			base = "default endpoint"
		}
		out = append(out, frontend.Check{Name: "llm", OK: true, Detail: "model " + infer.ModelName() + " via " + base + " (credentials set)"})
	} else {
		out = append(out, frontend.Check{Name: "llm", OK: true, Detail: "not configured (offline mode; set ANTHROPIC_AUTH_TOKEN to enable --llm)"})
	}
	cache := cacheDir
	if cache == "" {
		cache = defaultCacheRoot(repo)
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		out = append(out, frontend.Check{Name: "cache", Detail: cache + " not writable: " + err.Error()})
	} else {
		out = append(out, frontend.Check{Name: "cache", OK: true, Detail: cache})
	}
	return out
}

// newVersionCmd version：版本与构建信息。
func newVersionCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version and build information",
		Long: `Print the SpecForge version, the Go runtime it was built with and the VCS revision.
The version is part of every cache key: upgrading SpecForge invalidates cached runs.`,
		Example: `  specforge version
  specforge version --json`,
		Args: cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			info := map[string]string{"version": Version, "go": runtime.Version()}
			if bi, ok := debug.ReadBuildInfo(); ok {
				for _, s := range bi.Settings {
					if s.Key == "vcs.revision" || s.Key == "vcs.modified" {
						info[s.Key] = s.Value
					}
				}
			}
			a.emit(info, func(w io.Writer) {
				fmt.Fprintf(w, "specforge %s\n  go: %s\n", Version, info["go"])
				if rev := info["vcs.revision"]; rev != "" {
					fmt.Fprintf(w, "  revision: %s (modified: %s)\n", rev, info["vcs.modified"])
				}
			})
		},
	}
}
