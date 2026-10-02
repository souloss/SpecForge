package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/specforge/specforge/internal/compiler"
	"github.com/specforge/specforge/internal/engine"
	"github.com/specforge/specforge/internal/infer"
)

// 缓存目录布局：<cache-dir>/memo（整次运行产物）与 <cache-dir>/llm（LLM 调用）。
const (
	memoSubdir = "memo"
	llmSubdir  = "llm"
)

// defaultLLMBudget --llm-budget 默认值：单次运行最多发起的「未命中缓存」兜底调用数。
const defaultLLMBudget = 20

// defaultLLMConcurrency --llm-concurrency 默认值：并发调用数（写回按序，结果与并发数无关）。
const defaultLLMConcurrency = 4

// legacyMemoOff / legacyMemoAuto 旧版 --memo 取值（隐藏兼容）。
const (
	legacyMemoOff  = "off"
	legacyMemoAuto = "auto"
)

// 日志级别取值（--log-level）。
const (
	levelError = "error"
	levelWarn  = "warn"
	levelInfo  = "info"
	levelDebug = "debug"
)

// 日志格式取值（--log-format）。
const (
	formatText = "text"
	formatJSON = "json"
)

// 选项分组标题（帮助分节）。
const (
	groupInput   = "Input"
	groupOutput  = "Output & cache"
	groupLLM     = "LLM gap filling"
	groupCI      = "CI"
	groupLogging = "Logging (stderr)"
)

// envHelp 生成相关的环境变量说明（gen 帮助末尾）。
const envHelp = `Environment:
  ANTHROPIC_AUTH_TOKEN / ANTHROPIC_API_KEY   LLM credentials (required by --llm, --llm-enrich, --llm-learn-profile)
  ANTHROPIC_BASE_URL                         Anthropic-protocol edge endpoint (default: official endpoint)
  ANTHROPIC_MODEL                            model ID passed to the edge (default: deepseek-v4-pro-0813)
  SPECFORGE_LLM_TIMEOUT                      per-call timeout incl. tool turns, Go duration (default: 180s)
  SPECFORGE_LLM_THINKING=off                 disable the model's extended thinking (faster, cheaper)`

// genOpts gen 命令选项。
type genOpts struct {
	repo, service, serviceRoot, profile, out string   // 输入仓库、服务过滤、服务源码根、画像文件、产物目录
	openapi                                  []string // 已有 OpenAPI 契约，可重复指定
	runtime                                  []string // 脱敏 JSONL runtime observation，可重复指定
	documentation                            []string // 结构化人工契约，可重复指定
	frontend                                 string   // 语言前端名；空 = 按仓库自动识别
	cacheDir                                 string   // 缓存根目录（空 = <repo>/.specforge/cache）
	noCache                                  bool     // 禁用全部缓存
	llm                                      bool     // 启用 LLM 兜底
	llmBudget                                int      // 未命中缓存的兜底调用上限
	llmConcurrency                           int      // LLM 并发数
	learnProfile                             bool     // LLM 画像学习
	enrich                                   bool     // LLM 语义增强
	failOnDegraded                           bool     // 降级/预算耗尽时以非 0 退出（CI 闸门）
	failOnConflict                           bool     // Contract Graph 存在冲突时失败
	failOnUnresolved                         bool     // Contract Graph 存在未解析项时失败
	minConfidence                            float64  // operation 最低置信度
	logLevel                                 string   // 日志级别
	logFormat                                string   // 日志格式
	quiet                                    bool     // 只输出错误日志
	legacyMemo                               string   // 旧版 --memo（隐藏兼容）
	legacyVerbose                            bool     // 旧版 --verbose（隐藏兼容，等价 --log-level=debug）
}

// newGenCmd gen：分析仓库并生成 openapi.yaml / report.md / operations.json。
func newGenCmd(a *app) *cobra.Command {
	o := &genOpts{}
	cmd := &cobra.Command{
		Use:   "gen",
		Short: "Generate OpenAPI 3.1 from a repository",
		Long: `Analyze a repository and generate its OpenAPI 3.1 contract. No annotations are needed.

Go repositories use the static frontend: routes, middleware, request binding, parameters, response
envelopes and error codes are traced through the call graph with full type information. Any other
source tree (or --frontend generic) uses the generic LLM frontend: routes and contracts are read by
the LLM file by file and every answer is checked against the source text (path literal and handler
name on the registration line, field names present in the handler), with confidence capped at 0.6.
It requires --llm.

Outputs (in --out, default <repo>/.specforge/out):
  openapi.yaml      the spec; byte-identical for identical inputs, evidence paths are repo-relative
  report.md         low-confidence operations with their gaps and evidence, plus an overview table
  operations.json   per-operation evidence view (read by 'ops' and 'explain', stable JSON contract)

Caching (in --cache-dir, default <repo>/.specforge/cache):
  memo/  whole-run cache: unchanged sources + profile + engine + LLM settings reuse all outputs
  llm/   per-call LLM cache keyed by prompt content; tool reads are replayed against the current code
  facts.sqlite  normalized Contract Graph snapshots and operation dependency tokens
         and a changed read invalidates the entry, so only operations whose code changed are re-asked

LLM gap filling (--llm):
  Only two kinds of static gaps are sent: unresolved error codes and 'any' success payloads. The
  model receives the operation's call-graph source slice; every answer is verified against the code
  (an error code must appear in the operation's reachable source, a field must be a statically
  observed key) and rejected otherwise. --llm-budget limits new, uncached calls only, so repeated runs
  with the same budget make progress while cached operations stay free.

` + envHelp,
		Example: `  # whole repository, default output directory
  specforge gen --repo ./svc

  # one service of a monorepo, output into the docs folder
  specforge gen --repo . --service service-ipo --out docs/api

  # fill static gaps with an LLM, up to 50 new calls, 8 in parallel
  specforge gen --repo . --llm --llm-budget 50 --llm-concurrency 8

  # a non-Go service (Express, Flask, Spring, ...) through the generic LLM frontend
  specforge gen --repo ./node-svc --llm

  # CI: machine-readable summary, fail if LLM calls failed or the budget left gaps
  specforge gen --repo . --llm --fail-on-degraded --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log, err := o.logger(a.stderr, a.json)
			if err != nil {
				return err
			}
			cfg, err := o.config()
			if err != nil {
				return err
			}
			cfg.Logger = log
			res, err := engine.Run(cmd.Context(), cfg)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return newErr(exitCanceled, codeCanceled, "operation canceled", "")
				}
				if errors.Is(err, engine.ErrConfig) {
					return newErr(exitConfig, codeConfigInvalid, err.Error(), "point --repo at a project root (Go module, or any source tree with --llm for the generic frontend) and make sure the profile is valid YAML ('specforge doctor' checks both)")
				}
				return newErr(exitInternal, codeInternal, err.Error(), "")
			}
			if !res.Cached && !compiler.CompileTwiceCheck(res.Doc) {
				return newErr(exitInternal, codeDeterminism, "determinism self-check failed: two compiles produced different bytes", "please report this as a bug")
			}
			a.emit(res, func(w io.Writer) { printGenSummary(w, res) })
			return o.exitFor(res)
		},
	}
	f := cmd.Flags()
	f.SortFlags = false // 帮助按注册顺序分组展示：Input → Output → LLM → CI → Logging
	f.StringVar(&o.repo, "repo", ".", "repository root; must contain go.mod")
	f.StringVar(&o.service, "service", "", "only analyze the service whose main package is cmd/<name> (names: 'specforge doctor'); empty = whole repository")
	f.StringVar(&o.serviceRoot, "service-root", "", "generic frontend source root relative to --repo; empty = whole repository")
	f.StringVar(&o.profile, "profile", "", "convention profile YAML: sinks, auth middleware, wildcard expansion (default: <repo>/.specforge/profile.yaml if present, else built-in defaults + auto-discovery)")
	f.StringVar(&o.frontend, "frontend", "", "language frontend: go | generic (default: auto-detect; 'generic' is the LLM-only frontend for unsupported languages, needs --llm)")
	f.StringArrayVar(&o.openapi, "openapi", nil, "existing OpenAPI document to ingest; repeat for multiple documents")
	f.StringArrayVar(&o.runtime, "runtime", nil, "redacted JSONL runtime observations to ingest; repeat for multiple files")
	f.StringArrayVar(&o.documentation, "documentation", nil, "structured documentation facts to ingest; repeat for multiple files")
	setGroup(f, groupInput, "repo", "service", "service-root", "profile", "frontend", "openapi", "runtime", "documentation")

	f.StringVarP(&o.out, "out", "o", "", "output directory (default: <repo>/.specforge/out)")
	f.StringVar(&o.cacheDir, "cache-dir", "", "cache root holding memo/ and llm/ (default: <repo>/.specforge/cache)")
	f.BoolVar(&o.noCache, "no-cache", false, "disable both caches: always re-analyze and re-ask the LLM")
	setGroup(f, groupOutput, "out", "cache-dir", "no-cache")

	f.BoolVar(&o.llm, "llm", false, "fill static gaps (unresolved error codes, 'any' payloads) with an LLM; answers are verified against the code")
	f.IntVar(&o.llmBudget, "llm-budget", defaultLLMBudget, "max new (uncached) LLM gap-filling calls per run; cached operations are free; 0 = unlimited")
	f.IntVar(&o.llmConcurrency, "llm-concurrency", defaultLLMConcurrency, "parallel LLM calls; output is identical for any value")
	f.BoolVar(&o.enrich, "llm-enrich", false, "let the LLM write summary/description for operations without doc comments")
	f.BoolVar(&o.learnProfile, "llm-learn-profile", false, "let the LLM learn repo conventions from sample handlers; only fills profile entries that are missing")
	setGroup(f, groupLLM, "llm", "llm-budget", "llm-concurrency", "llm-enrich", "llm-learn-profile")

	f.BoolVar(&o.failOnDegraded, "fail-on-degraded", false, "exit 10 if LLM calls or operation analyses failed, 20 if the LLM budget left gaps (outputs are still written)")
	f.BoolVar(&o.failOnConflict, "fail-on-conflict", false, "fail if merged source candidates disagree")
	f.BoolVar(&o.failOnUnresolved, "fail-on-unresolved", false, "fail if unresolved routes, schemas, or operation gaps remain")
	f.Float64Var(&o.minConfidence, "min-confidence", 0, "fail if any operation confidence is below this value (0 disables the gate)")
	setGroup(f, groupCI, "fail-on-degraded", "fail-on-conflict", "fail-on-unresolved", "min-confidence")

	f.StringVar(&o.logLevel, "log-level", "", "error|warn|info|debug (default: info on a terminal, warn otherwise)")
	f.StringVar(&o.logFormat, "log-format", formatText, "text|json")
	f.BoolVarP(&o.quiet, "quiet", "q", false, "only log errors (same as --log-level=error)")
	setGroup(f, groupLogging, "log-level", "log-format", "quiet")

	f.StringVar(&o.legacyMemo, "memo", "", "deprecated: use --cache-dir / --no-cache")
	f.BoolVar(&o.legacyVerbose, "verbose", false, "deprecated: use --log-level=debug")
	_ = f.MarkHidden("memo")
	_ = f.MarkHidden("verbose")
	return cmd
}

// logger 按日志选项构造 stderr 日志器（机器模式默认 warn，避免刷屏）。
func (o *genOpts) logger(stderr io.Writer, machine bool) (*slog.Logger, error) {
	level := o.logLevel
	switch {
	case o.quiet:
		level = levelError
	case level == "" && o.legacyVerbose:
		level = levelDebug
	case level == "":
		level = levelWarn
		if isTerminal(os.Stderr) && !machine {
			level = levelInfo
		}
	}
	var lv slog.Level
	switch level {
	case levelError:
		lv = slog.LevelError
	case levelWarn:
		lv = slog.LevelWarn
	case levelInfo:
		lv = slog.LevelInfo
	case levelDebug:
		lv = slog.LevelDebug
	default:
		return nil, newErr(exitUsage, codeInvalidArgs, "invalid --log-level "+level, "use one of: error, warn, info, debug")
	}
	opts := &slog.HandlerOptions{Level: lv}
	switch o.logFormat {
	case formatText:
		return slog.New(slog.NewTextHandler(stderr, opts)), nil
	case formatJSON:
		return slog.New(slog.NewJSONHandler(stderr, opts)), nil
	}
	return nil, newErr(exitUsage, codeInvalidArgs, "invalid --log-format "+o.logFormat, "use text or json")
}

// config 选项 → 引擎配置（含缓存目录解析与 LLM provider 构造）。
func (o *genOpts) config() (engine.Config, error) {
	if o.llmBudget < 0 || o.llmConcurrency < 0 {
		return engine.Config{}, newErr(exitUsage, codeInvalidArgs, "--llm-budget and --llm-concurrency must be >= 0", "")
	}
	if o.minConfidence < 0 || o.minConfidence > 1 {
		return engine.Config{}, newErr(exitUsage, codeInvalidArgs, "--min-confidence must be between 0 and 1", "")
	}
	if o.service != "" && o.frontend == "generic" && o.serviceRoot == "" {
		return engine.Config{}, newErr(exitUsage, codeInvalidArgs, "--service-root is required with --frontend generic --service", "point --service-root at the service source directory")
	}
	cfg := engine.Config{
		RepoDir: o.repo, Service: o.service, ServiceRoot: o.serviceRoot, ProfilePath: o.profile, OutDir: o.out, OpenAPIFiles: append([]string(nil), o.openapi...), RuntimeFiles: append([]string(nil), o.runtime...), DocumentationFiles: append([]string(nil), o.documentation...),
		FailOnConflict: o.failOnConflict, FailOnUnresolved: o.failOnUnresolved, MinConfidence: o.minConfidence,
		LLMBudget: o.llmBudget, LLMConcurrency: o.llmConcurrency,
		LearnProfile: o.learnProfile, Enrich: o.enrich,
	}
	if o.frontend != "" {
		fe, err := engine.FrontendByName(o.frontend)
		if err != nil {
			return cfg, newErr(exitUsage, codeInvalidArgs, err.Error(), "use one of: "+strings.Join(engine.SupportedLanguages(), ", "))
		}
		cfg.Frontend = fe
	}
	if root := o.cacheRoot(); root != "" {
		cfg.MemoDir = filepath.Join(root, memoSubdir)
		cfg.LLMCacheDir = filepath.Join(root, llmSubdir)
		cfg.FactCacheDir = filepath.Join(root, "facts.sqlite")
	}
	if o.llm || o.learnProfile || o.enrich {
		cfg.Provider = infer.NewProviderFromEnv()
		if cfg.Provider == nil {
			return cfg, newErr(exitConfig, codeLLMNotConfig, "LLM requested but no credentials configured",
				"set ANTHROPIC_AUTH_TOKEN (or ANTHROPIC_API_KEY), optionally ANTHROPIC_BASE_URL and ANTHROPIC_MODEL")
		}
	}
	return cfg, nil
}

// cacheRoot 解析缓存根目录（兼容旧版 --memo）；空 = 禁用缓存。
func (o *genOpts) cacheRoot() string {
	switch {
	case o.noCache, o.legacyMemo == legacyMemoOff:
		return ""
	case o.cacheDir != "":
		return o.cacheDir
	case o.legacyMemo != "" && o.legacyMemo != legacyMemoAuto:
		return o.legacyMemo
	}
	return defaultCacheRoot(o.repo)
}

// defaultCacheRoot 仓库默认缓存根（不参与输入指纹）。
func defaultCacheRoot(repo string) string {
	return filepath.Join(repo, ".specforge", "cache")
}

// defaultOutDir 仓库默认产物目录。
func defaultOutDir(repo string) string {
	return filepath.Join(repo, ".specforge", "out")
}

// exitFor 运行结果 → 退出码（仅 --fail-on-degraded 时把降级/预算耗尽映射为非 0）。
func (o *genOpts) exitFor(res *engine.Result) error {
	if !o.failOnDegraded {
		return nil
	}
	switch {
	case res.Degraded():
		return newErr(exitDegraded, codeLLMDegraded, fmt.Sprintf("degraded run: %d LLM failures, %d operation failures", res.LLM.Failed, len(res.OpFailures)), "see report.md; re-run to retry failed LLM calls (successful ones are cached)")
	case res.BudgetExhausted():
		return newErr(exitBudgetExceeded, codeBudgetExceeded, fmt.Sprintf("LLM budget exhausted: %d operations with gaps were not processed", res.LLM.BudgetSkipped), "raise --llm-budget or re-run (processed operations are cached and free)")
	}
	return nil
}

// printGenSummary 人类模式摘要。
func printGenSummary(w io.Writer, r *engine.Result) {
	cache := ""
	if r.Cached {
		cache = "  (run cache hit)"
	} else if r.GraphCached {
		cache = "  (Contract Graph cache hit)"
	}
	fmt.Fprintf(w, "specforge %s — %s%s\n", Version, r.Service, cache)
	fmt.Fprintf(w, "  routes        %d extracted, %d resolved, %d unique, %d collapsed, %d unresolved\n", r.Routes, r.RoutesResolved, r.RoutesUnique, r.RoutesCollapsed, r.RoutesUnresolved)
	for _, issue := range r.UnresolvedRoutes {
		location := issue.File
		if issue.Line > 0 {
			location = fmt.Sprintf("%s:%d", issue.File, issue.Line)
		}
		fmt.Fprintf(w, "                unresolved %s %s (%s: %s)\n", issue.Method, issue.RawPath, location, issue.Reason)
	}
	fmt.Fprintf(w, "  operations    %d   schemas %d   facts %d (dropped by evidence gate: %d)\n", r.Operations, r.SchemaTypes, r.Facts, r.Dropped)
	fmt.Fprintf(w, "  confidence    %d low-confidence operations, %d with remaining gaps\n", r.LowConf, r.RemainingGaps)
	if r.LLM.Attempted > 0 || r.LLM.Enriched > 0 || r.LLMCache.Hits+r.LLMCache.Misses > 0 {
		fmt.Fprintf(w, "  llm           %d ops: %d facts accepted, %d rejected by verification, %d failed, %d skipped by budget\n",
			r.LLM.Attempted, r.LLM.Accepted, r.LLM.Rejected, r.LLM.Failed, r.LLM.BudgetSkipped)
		fmt.Fprintf(w, "                %d calls, %d in + %d out tokens; cache %d hit / %d miss / %d stale (saved %d tokens)\n",
			r.LLMCalls, r.LLMUsage.InputTokens, r.LLMUsage.OutputTokens,
			r.LLMCache.Hits, r.LLMCache.Misses, r.LLMCache.Stale, r.LLMCache.SavedTokens)
	}
	if ad := r.LLM.Adapter; ad.Attempted > 0 {
		fmt.Fprintf(w, "  llm adapter   %d accepted, %d rejected by verification, %d failed (see .specforge/adapters/)\n", ad.Accepted, ad.Rejected, ad.Failed)
	}
	if ws := r.LLM.Wrappers; ws.Attempted > 0 {
		fmt.Fprintf(w, "  llm wrappers  %d summarized: %d accepted, %d rejected by verification, %d failed\n", ws.Attempted, ws.Accepted, ws.Rejected, ws.Failed)
	}
	if rs := r.LLM.Routes; rs.Attempted > 0 || rs.Failed > 0 {
		fmt.Fprintf(w, "  llm routes    %d reported: %d accepted, %d rejected by text verification, %d files failed\n", rs.Attempted, rs.Accepted, rs.Rejected, rs.Failed)
	}
	if cs := r.LLM.Contracts; cs.Attempted > 0 {
		fmt.Fprintf(w, "  llm contracts %d operations: %d fully verified, %d partly rejected, %d failed\n", cs.Attempted, cs.Accepted, cs.Rejected, cs.Failed)
	}
	if len(r.OpFailures) > 0 {
		fmt.Fprintf(w, "  failures      %d operations isolated after analysis errors (see report)\n", len(r.OpFailures))
	}
	var stages []string
	for _, s := range r.Stages {
		stages = append(stages, fmt.Sprintf("%s %dms", s.Name, s.Millis))
	}
	fmt.Fprintf(w, "  time          %dms  [%s]\n", r.ElapsedMs, strings.Join(stages, ", "))
	fmt.Fprintf(w, "  output        %s\n", r.Spec)
	fmt.Fprintf(w, "                %s\n", r.Report)
	if r.LowConf > 0 {
		fmt.Fprintf(w, "next: 'specforge ops --low-confidence -o %s' to list them, 'specforge explain METHOD PATH' for evidence\n", r.OutDir)
	}
}
