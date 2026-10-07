// Package engine 编排生成流水线（设计文档 §6.1 首次全量 12 步的 P0 实现）。
//
// 流程: 指纹/memo → 摄入 → 服务拓扑 → 框架识别 → 画像 → 路由抽取 →
// handler 分析（请求绑定/参数/中间件） → 响应追踪（切片+汇聚点） →
// Schema 合成 → 横切合成（security/错误码） → LLM 兜底（可选） →
// 证据闸门 → 确定性编译 → 输出。
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/specforge/specforge/internal/compiler"
	"github.com/specforge/specforge/internal/contract"
	"github.com/specforge/specforge/internal/docsource"
	"github.com/specforge/specforge/internal/factcache"
	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/frontend"
	"github.com/specforge/specforge/internal/frontend/generic"
	"github.com/specforge/specforge/internal/frontend/golang"
	"github.com/specforge/specforge/internal/infer"
	"github.com/specforge/specforge/internal/memo"
	"github.com/specforge/specforge/internal/openapi"
	"github.com/specforge/specforge/internal/runtime"
	"github.com/specforge/specforge/internal/schema"
)

// Version 引擎版本。分析行为（适配器/类型映射/编译）或依赖版本变化时须递增，
// 否则 memo 指纹会把旧引擎的缓存误判为新鲜（设计文档 §5.2 缓存键含 EngineVersion）。
const Version = "0.5.0"

// Config 生成配置。
type Config struct {
	RepoDir     string // 目标仓库根（含 go.mod）
	Service     string // 服务过滤名；空 = 全仓全部服务
	ServiceRoot string // 可选服务源码根（相对 RepoDir 或绝对路径，主要用于 generic 前端）
	ProfilePath string // 约定画像文件；空 = <repo>/.specforge/profile.yaml 或内置默认
	OutDir      string // 产物目录；空 = <repo>/.specforge/out
	// OpenAPIFiles 是额外的已有 OpenAPI 契约输入。它们在编译前导入
	// Contract Graph，并保留 document/jsonPath 证据。
	OpenAPIFiles []string
	// RuntimeFiles are redacted JSONL observation inputs.
	RuntimeFiles []string
	// DocumentationFiles are structured human-authored contract inputs.
	DocumentationFiles []string
	// Quality gates applied after all sources are merged.
	FailOnConflict   bool
	FailOnUnresolved bool
	MinConfidence    float64
	// MemoDir 运行级 memo 缓存目录；空 = 禁用（每次全量重算）。
	MemoDir      string
	FactCacheDir string // optional SQLite normalized graph cache
	// LLMCacheDir LLM 调用缓存目录（内容寻址 + 读集校验）；空 = 禁用。
	LLMCacheDir string
	// Provider 可选 LLM 供应方；非 nil 时对静态缺口做档位2/3 兜底采集。
	Provider infer.Provider
	// LLMBudget 单次运行 LLM 兜底的 operation 上限（0 = 不限）。
	LLMBudget int
	// LLMConcurrency LLM 调用并发数（<=1 串行）；写回按原顺序，结果与并发数无关。
	LLMConcurrency int
	// LearnProfile 用 LLM 从抽样接口源码归纳仓库约定（仅填补画像缺失项），需 Provider。
	LearnProfile bool
	// Enrich 用 LLM 为无 godoc 的 operation 生成 summary/description（F8），需 Provider。
	Enrich bool
	// Critic 用独立 LLM 对照源码审查每个 operation 的契约完整性（missing 参数/响应/状态/信封），
	// 需 Provider；产出过 verify 后作为 LLM 候选事实合并，不擅自改静态事实。
	Critic bool
	// RaiseConfidence 允许 critic 二次确认且证据完整时提升 LLM 事实核对强度（突破 text 0.6 顶）；
	// 保守默认关闭。
	RaiseConfidence bool
	// Logger 进度与诊断日志（nil = 丢弃）；CLI 按 -v 级别配置输出到 stderr。
	Logger *slog.Logger
	// Frontend 语言前端；nil = 按仓库自动识别（见 frontends）。
	Frontend frontend.Frontend
}

// ErrConfig 配置/输入类错误（仓库不是受支持语言的项目、画像不合法等）：CLI 据此返回配置错误退出码。
var ErrConfig = frontend.ErrConfig

// frontends 内置语言前端（按识别优先级）。接入新语言：实现 frontend.Frontend 并加入此表（通用 LLM 前端须保持最后：
// 它对任何含源码的仓库都匹配，只在没有静态前端认领时接手）。
var frontends = []frontend.Frontend{golang.New(), generic.New()}

// FrontendByName 按名取内置前端（--frontend 取值）；未知名字返回 ErrConfig。
func FrontendByName(name string) (frontend.Frontend, error) {
	for _, fe := range frontends {
		if fe.Name() == name {
			return fe, nil
		}
	}
	return nil, fmt.Errorf("%w: unknown frontend %q (available: %s)", ErrConfig, name, strings.Join(SupportedLanguages(), ", "))
}

// pickFrontend 选择语言前端：显式配置优先，否则按仓库识别。
func pickFrontend(cfg Config) (frontend.Frontend, error) {
	if cfg.Frontend != nil {
		return cfg.Frontend, nil
	}
	return DetectFrontend(cfg.RepoDir)
}

// DetectFrontend 按仓库识别语言前端；都不匹配时返回 ErrConfig（列出支持的语言）。
func DetectFrontend(repoDir string) (frontend.Frontend, error) {
	for _, fe := range frontends {
		if fe.Detect(repoDir) {
			return fe, nil
		}
	}
	return nil, fmt.Errorf("%w: %s is not a project of a supported language (%s)", ErrConfig, repoDir, strings.Join(SupportedLanguages(), ", "))
}

// SupportedLanguages 内置语言前端名。
func SupportedLanguages() []string {
	names := make([]string, len(frontends))
	for i, fe := range frontends {
		names[i] = fe.Name()
	}
	return names
}

// lowConfThreshold 低置信判定线：operation 置信度低于此值即进入报告明细与 low-confidence 计数。
const lowConfThreshold = 0.8

// Stage 一个流水线阶段的耗时（可观测性：定位慢在哪）。
type Stage struct {
	Name   string `json:"name"` // 阶段名
	Millis int64  `json:"ms"`   // 耗时（毫秒）
}

// OpFailure 单个 operation 分析失败（前端隔离，不影响其它 operation）。
type OpFailure = frontend.OpFailure

// Result 一次运行的统计与产物（CLI、--json 与 memo 摘要共用；Doc/FactsList 不序列化）。
type Result struct {
	Service          string                `json:"service"`           // 服务名（全仓模式为模块路径）
	Repo             string                `json:"repo"`              // 仓库目录
	Fingerprint      string                `json:"fingerprint"`       // 输入指纹（memo 键）
	Cached           bool                  `json:"cached"`            // 命中运行级 memo（本次未重新分析）
	GraphCached      bool                  `json:"graph_cached"`      // 命中 SQLite Contract Graph 快照
	Packages         int                   `json:"packages"`          // 分析的包数
	Symbols          int                   `json:"symbols"`           // 符号数
	CallEdges        int                   `json:"call_edges"`        // 调用边数
	Routes           int                   `json:"routes"`            // 抽取的路由数（展开前）
	RoutesResolved   int                   `json:"routes_resolved"`   // 可生成 operation 的路由数（展开/模板化后）
	RoutesUnresolved int                   `json:"routes_unresolved"` // 仍未解析的路由数
	RoutesUnique     int                   `json:"routes_unique"`     // method + path 去重后的 operation 身份数
	RoutesCollapsed  int                   `json:"routes_collapsed"`  // 重复 method + path 注册折叠数
	UnresolvedRoutes []frontend.RouteIssue `json:"unresolved_routes"`
	// MissedRoutes 通用前端 recall 守卫的疑似遗漏路由（只进报告/CI，不进 spec）。
	MissedRoutes []frontend.RouteIssue `json:"missed_routes"`
	// RuntimeOrphans runtime 观察到、但静态/文档来源从未出现的 route（recall 补采信号，只进报告）。
	RuntimeOrphans   []string              `json:"runtime_orphans,omitempty"`
	Operations       int                   `json:"operations"`      // 输出的 operation 数
	SchemaTypes      int                   `json:"schema_types"`    // schema 类型数
	SinkSites        int                   `json:"sink_sites"`      // 事实构造阶段产出的事实数（旧口径，保留兼容）
	Facts            int                   `json:"facts"`           // 通过证据闸门的事实数
	Dropped          int                   `json:"dropped"`         // 证据闸门丢弃的事实数
	DroppedIDs       []string              `json:"dropped_ids"`     // 被丢弃事实的 ID（报告列出，便于排查证据缺失）
	LowConf          int                   `json:"low_confidence"`  // 低置信 operation 数
	Gaps             []string              `json:"gaps"`            // LLM 兜底前的静态缺口（"METHOD /path: gap1; gap2"）
	RemainingGaps    int                   `json:"remaining_gaps"`  // 最终仍有缺口的 operation 数
	Conflicts        int                   `json:"conflicts"`       // Contract Graph 中保留的候选冲突数
	UnknownSchemas   int                   `json:"unknown_schemas"` // 显式未知 schema 数
	GraphDiagnostics []contract.Diagnostic `json:"graph_diagnostics,omitempty"`
	OpFailures       []OpFailure           `json:"op_failures"` // 分析失败被隔离的 operation
	LLM              llmStats              `json:"llm"`         // LLM 兜底统计
	LLMCalls         int                   `json:"llm_calls"`   // 实际发生的模型调用次数（缓存命中不计）
	LLMUsage         infer.Usage           `json:"llm_usage"`   // 实际 token 用量
	LLMCache         infer.CacheStats      `json:"llm_cache"`   // LLM 调用缓存统计
	Stages           []Stage               `json:"stages"`      // 各阶段耗时
	ElapsedMs        int64                 `json:"elapsed_ms"`  // 总耗时
	OutDir           string                `json:"out_dir"`     // 产物目录
	Spec             string                `json:"spec"`        // openapi.yaml 路径
	Report           string                `json:"report"`      // report.md 路径

	Doc       *compiler.Document `json:"-"` // 编译产物（缓存命中时为 nil）
	FactsList []*facts.Fact      `json:"-"` // 全部事实（缓存命中时为 nil）
}

// Degraded 本次运行是否部分降级：LLM 调用失败，或有 operation 分析失败被隔离。
func (r *Result) Degraded() bool { return r.LLM.Failed > 0 || len(r.OpFailures) > 0 }

// BudgetExhausted 是否因 LLM 预算上限留下了未处理的缺口。
func (r *Result) BudgetExhausted() bool { return r.LLM.BudgetSkipped > 0 }

// stageTimer 阶段计时器：begin 记录起点，返回的函数在阶段结束时调用。
type stageTimer struct {
	res *Result
	log *slog.Logger
}

// begin 开始一个阶段；返回的结束函数记录耗时并输出 debug 日志。
func (t stageTimer) begin(name string) func() {
	t.log.Debug("阶段开始", "stage", name)
	start := time.Now()
	return func() {
		ms := time.Since(start).Milliseconds()
		t.res.Stages = append(t.res.Stages, Stage{Name: name, Millis: ms})
		t.log.Info("阶段完成", "stage", name, "ms", ms)
	}
}

// Artifact file names.
const (
	specFile       = "openapi.yaml"    // OpenAPI 产物
	reportFile     = "report.md"       // 置信度报告
	operationsFile = "operations.json" // operation 级证据视图（explain 与 agent 消费）
	contractFile   = "contract.json"   // 规范化 Contract Graph（候选、来源与诊断）
	summaryFile    = "summary.json"    // 运行摘要（memo 命中时回填 Result）
)

// Run 执行完整生成。ctx 取消时尽快返回（LLM 阶段停止派发，已完成结果照常写回）。
func Run(ctx context.Context, cfg Config) (*Result, error) {
	t0 := time.Now()
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	res := &Result{Repo: cfg.RepoDir}
	timer := stageTimer{res: res, log: log}
	outDir := cfg.OutDir
	if outDir == "" {
		outDir = filepath.Join(cfg.RepoDir, ".specforge", "out")
	}
	res.OutDir, res.Spec, res.Report = outDir, filepath.Join(outDir, specFile), filepath.Join(outDir, reportFile)

	fe, err := pickFrontend(cfg)
	if err != nil {
		return nil, err
	}

	// 0. 运行级 memo：输入指纹未变直接复用上次产物，跳过整个分析管线。
	end := timer.begin("fingerprint")
	inputs := append(append(append([]string(nil), cfg.OpenAPIFiles...), cfg.RuntimeFiles...), cfg.DocumentationFiles...)
	qualityKey := fmt.Sprintf("quality:conflict=%v:unresolved=%v:min=%.6f:service-root=%s", cfg.FailOnConflict, cfg.FailOnUnresolved, cfg.MinConfidence, cfg.ServiceRoot)
	fp, err := memo.FingerprintWithInputs(cfg.RepoDir, cfg.Service, cfg.ProfilePath, engineBuildKey()+"|"+fe.Name()+"|"+qualityKey, llmKeyOf(cfg), inputs, fe.IsSource)
	end()
	if err != nil {
		return nil, err
	}
	res.Fingerprint = fp
	store := memo.New(cfg.MemoDir)
	if cached, ok := loadMemo(store, fp, outDir); ok {
		cached.Cached, cached.Stages, cached.OutDir, cached.Spec, cached.Report = true, res.Stages, res.OutDir, res.Spec, res.Report
		cached.LLMCalls, cached.LLMUsage, cached.LLMCache = 0, infer.Usage{}, infer.CacheStats{}
		cached.ElapsedMs = time.Since(t0).Milliseconds()
		log.Info("memo 命中，复用上次产物", "fingerprint", fp[:12])
		return cached, nil
	}

	// LLM 供应方：包一层内容寻址调用缓存（读集校验）。
	provider := infer.NewCachedProvider(cfg.Provider, cfg.LLMCacheDir)

	// 1-5. 语言前端：摄入 → 程序模型 → 路由 → 契约/响应/schema 事实（前端内部阶段各自计时）。
	an, err := fe.Analyze(ctx, frontend.Request{
		RepoDir: cfg.RepoDir, Service: cfg.Service, ServiceRoot: cfg.ServiceRoot,
		Manifest: frontend.ServiceManifest{Name: cfg.Service, Root: cfg.ServiceRoot}, ProfilePath: cfg.ProfilePath,
		Provider: provider, LearnProfile: cfg.LearnProfile, Concurrency: cfg.LLMConcurrency, Log: log, Stage: timer.begin,
	})
	if err != nil {
		return nil, err
	}
	res.Service, res.OpFailures = an.Service, an.OpFailures
	res.Packages, res.Symbols, res.CallEdges = an.Stats.Packages, an.Stats.Symbols, an.Stats.CallEdges
	res.Routes, res.RoutesResolved, res.RoutesUnresolved = an.Stats.Routes, an.Stats.RoutesResolved, an.Stats.RoutesUnresolved
	res.RoutesUnique, res.RoutesCollapsed = an.Stats.RoutesUnique, an.Stats.RoutesCollapsed
	res.UnresolvedRoutes = append([]frontend.RouteIssue{}, an.UnresolvedRoutes...)
	res.MissedRoutes = append([]frontend.RouteIssue{}, an.MissedRoutes...)
	res.SinkSites = an.Stats.SinkSites
	res.LLM.Wrappers, res.LLM.Adapter, res.LLM.Routes, res.LLM.Contracts = an.Wrappers, an.Adapter, an.Routes, an.Contracts
	factList := an.Facts
	res.Gaps = collectGaps(factList)
	log.Info("静态分析完成", "operations", len(an.Handlers), "gaps", len(res.Gaps))

	// 6. LLM 兜底（档位2/3）+ 语义增强：显式启用才执行；离线时缺口保持 unknown，绝不编造。
	if provider != nil {
		lp := newLLMPhase(an.Program, an.Catalog, an.SuccessCode, provider, log)
		lp.raiseConf = cfg.RaiseConfidence
		if len(res.Gaps) > 0 {
			end = timer.begin("llm-gaps")
			var llmSchemas []*facts.Fact
			var st llmStats
			llmSchemas, st = lp.resolveGaps(ctx, factList, an.Handlers, cfg.LLMBudget, cfg.LLMConcurrency)
			st.Wrappers, st.Adapter, st.Routes, st.Contracts = res.LLM.Wrappers, res.LLM.Adapter, res.LLM.Routes, res.LLM.Contracts
			res.LLM = st
			factList = append(factList, llmSchemas...)
			end()
		}
		if cfg.Enrich {
			end = timer.begin("llm-enrich")
			enriched := lp.enrichOperations(ctx, factList, an.Handlers, cfg.LLMConcurrency)
			res.LLM.Enriched = len(enriched)
			factList = append(factList, enriched...)
			end()
		}
		if cfg.Critic {
			end = timer.begin("llm-critic")
			criticFacts, cs := lp.criticizeOperations(ctx, factList, an.Handlers, cfg.LLMConcurrency)
			res.LLM.Critic = cs
			factList = append(factList, criticFacts...)
			end()
		}
		if ur, ok := provider.(infer.UsageReporter); ok {
			res.LLMUsage = ur.LLMUsage()
			res.LLMCalls = res.LLMUsage.Calls
		}
		if cp, ok := provider.(*infer.CachedProvider); ok {
			res.LLMCache = cp.CacheStats()
		}
	}

	// 7. 证据闸门：无有效证据的事实丢弃，证据路径相对化。
	end = timer.begin("evidence")
	factList, res.DroppedIDs = verifyEvidence(an.Program, factList)
	res.Dropped = len(res.DroppedIDs)
	end()

	// 8. 确定性编译
	compileEnd := timer.begin("compile")
	schemaFacts := map[string]*schema.Schema{}
	for _, f := range factList {
		if f.Kind == facts.KindSchema {
			if s, ok := f.Schema(); ok {
				schemaFacts[strings.TrimPrefix(f.ID, "schema:")] = s
			}
		}
	}
	res.SchemaTypes = len(schemaFacts)
	res.FactsList, res.Facts = factList, len(factList)
	graph := graphFromFacts(factList)
	graphCached := false
	factStore, factCacheErr := factcache.Open(cfg.FactCacheDir)
	if factCacheErr != nil {
		return nil, fmt.Errorf("open fact cache: %w", factCacheErr)
	}
	defer factStore.Close()
	if cachedGraph, hit, cacheErr := factStore.Load(fp, engineBuildKey()); cacheErr != nil {
		return nil, fmt.Errorf("load fact cache: %w", cacheErr)
	} else if hit {
		graph = cachedGraph
		graphCached = true
		res.GraphCached = true
	}
	if !graphCached && len(cfg.OpenAPIFiles) > 0 {
		ingestEnd := timer.begin("ingest-openapi")
		for _, path := range cfg.OpenAPIFiles {
			documentGraph, importErr := openapi.Import(path)
			if importErr != nil {
				ingestEnd()
				return nil, fmt.Errorf("%w: import OpenAPI %q: %v", ErrConfig, path, importErr)
			}
			mergeContractGraphs(graph, documentGraph)
		}
		ingestEnd()
	}
	if !graphCached && len(cfg.RuntimeFiles) > 0 {
		ingestEnd := timer.begin("ingest-runtime")
		for _, path := range cfg.RuntimeFiles {
			observationGraph, importErr := runtime.ImportObservations(path)
			if importErr != nil {
				ingestEnd()
				return nil, fmt.Errorf("%w: import runtime observations %q: %v", ErrConfig, path, importErr)
			}
			res.RuntimeOrphans = collectRuntimeOrphans(graph, observationGraph)
			mergeContractGraphs(graph, observationGraph)
		}
		ingestEnd()
	}
	if !graphCached && len(cfg.DocumentationFiles) > 0 {
		ingestEnd := timer.begin("ingest-documentation")
		for _, path := range cfg.DocumentationFiles {
			documentationGraph, importErr := docsource.Import(path)
			if importErr != nil {
				ingestEnd()
				return nil, fmt.Errorf("%w: import documentation %q: %v", ErrConfig, path, importErr)
			}
			mergeContractGraphs(graph, documentationGraph)
		}
		ingestEnd()
	}
	markGraphStates(graph)
	if err := factStore.SaveWithRoot(fp, engineBuildKey(), graph, cfg.RepoDir); err != nil {
		return nil, fmt.Errorf("save fact cache: %w", err)
	}
	res.Conflicts, res.UnknownSchemas = graphStats(graph)
	res.GraphDiagnostics = contract.ValidateGraph(graph)
	conflictDiagnostics, unresolvedDiagnostics := 0, 0
	for _, diagnostic := range res.GraphDiagnostics {
		if strings.Contains(diagnostic.Code, "conflict") {
			conflictDiagnostics++
		}
		if strings.Contains(diagnostic.Code, "unresolved") || strings.Contains(diagnostic.Code, "unknown") || strings.Contains(diagnostic.Code, "missing") {
			unresolvedDiagnostics++
		}
	}
	if cfg.FailOnConflict && (res.Conflicts > 0 || conflictDiagnostics > 0) {
		return nil, fmt.Errorf("contract graph validation failed: %d conflicts", res.Conflicts)
	}
	if cfg.FailOnUnresolved && (res.UnknownSchemas > 0 || res.RoutesUnresolved > 0 || graphHasGaps(graph) || unresolvedDiagnostics > 0) {
		return nil, fmt.Errorf("contract graph validation failed: unresolved routes, schemas, or operation gaps remain")
	}
	if cfg.MinConfidence > 0 {
		for _, operation := range graph.Operations {
			if operation != nil && operation.Confidence < cfg.MinConfidence {
				return nil, fmt.Errorf("contract graph validation failed: operation %s confidence %.3f is below %.3f", operation.Key, operation.Confidence, cfg.MinConfidence)
			}
		}
	}
	for _, f := range factList {
		// 按 operation 计：schema 事实会随引用它的每个 operation 重复出现，按事实计会虚高
		if f.Kind == facts.KindContract && f.Confidence < lowConfThreshold {
			res.LowConf++
		}
		if cp, ok := f.Contract(); ok && len(cp.Gaps) > 0 {
			res.RemainingGaps++
		}
	}
	doc, err := compiler.Compile(compiler.Input{
		ServiceName: res.Service, Framework: an.Framework,
		Graph: graph,
	})
	if err != nil {
		return nil, err
	}
	res.Doc, res.Operations = doc, len(doc.Operations)
	specYAML, err := compiler.RenderYAML(doc)
	compileEnd()
	if err != nil {
		return nil, err
	}
	// Keep generated output inside the OpenAPI adapter boundary. This catches
	// malformed references and spec violations before artifacts are published.
	generated, err := openapi.LoadBytes(specYAML, filepath.Join(outDir, specFile))
	if err != nil {
		return nil, fmt.Errorf("reload generated OpenAPI: %w", err)
	}
	defer generated.Close()
	if diagnostics := generated.Validate(); len(diagnostics) > 0 {
		return nil, fmt.Errorf("generated OpenAPI validation failed (framework %s, %d packages, %d routes, %d resolved, %d operations): %s", an.Framework, an.Stats.Packages, an.Stats.Routes, an.Stats.RoutesResolved, len(doc.Operations), formatOpenAPIDiagnostics(diagnostics))
	}

	// 9. 写产物 + memo
	end = timer.begin("write")
	report := renderReport(doc, res)
	opsJSON, err := operationsJSON(doc)
	if err != nil {
		return nil, err
	}
	graphJSON, err := graphJSON(graph)
	if err != nil {
		return nil, err
	}
	artifacts := map[string][]byte{specFile: specYAML, reportFile: []byte(report), operationsFile: opsJSON, contractFile: graphJSON}
	if err := writeArtifacts(outDir, artifacts); err != nil {
		return nil, err
	}
	res.ElapsedMs = time.Since(t0).Milliseconds()
	end()
	if !ctxDone(ctx) { // 被取消的运行结果不完整，不进 memo
		if err := saveMemo(store, fp, artifacts, res); err != nil {
			log.Warn("memo 写入失败（不影响本次产物）", "err", err)
		}
	}
	res.ElapsedMs = time.Since(t0).Milliseconds()
	return res, nil
}

func formatOpenAPIDiagnostics(diagnostics []openapi.Diagnostic) string {
	parts := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		location := ""
		if diagnostic.Path != "" {
			location = " at " + diagnostic.Path
		}
		if diagnostic.Line > 0 {
			location += fmt.Sprintf(" (line %d, column %d)", diagnostic.Line, diagnostic.Column)
		}
		parts = append(parts, diagnostic.Code+location+": "+diagnostic.Message)
	}
	return strings.Join(parts, "; ")
}

// writeArtifacts 写产物文件（目录不存在则创建）。
func writeArtifacts(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), files[n], 0o644); err != nil {
			return err
		}
	}
	return nil
}

// collectGaps 聚合静态缺口清单：把 contract 事实里的 Gaps 提取成
// "METHOD /path: gap1; gap2" 的可读行，供档位判定与 report 消费。
func collectGaps(factList []*facts.Fact) []string {
	var out []string
	for _, f := range factList {
		if f.Kind != facts.KindContract {
			continue
		}
		cp, ok := f.Contract()
		if !ok {
			continue
		}
		if len(cp.Gaps) == 0 {
			continue
		}
		method, path := opMethodPath(f.ID)
		out = append(out, method+" "+path+": "+strings.Join(cp.Gaps, "; "))
	}
	sort.Strings(out)
	return out
}

// opMethodPath 从 contract 事实 ID（"contract:op:METHOD:/path"）提取 method 与 path。
func opMethodPath(factID string) (string, string) {
	s := factID
	s = strings.TrimPrefix(s, "contract:")
	s = strings.TrimPrefix(s, "op:")
	if i := strings.Index(s, ":"); i > 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}

// llmKeyOf 本次运行的 LLM 配置指纹（计入 memo 指纹）：LLM 产出不来自仓库文件，
// 必须计入，否则离线/LLM 运行会互相命中脏缓存。并发数不影响产物（按序写回），不计入。
func llmKeyOf(cfg Config) string {
	if cfg.Provider == nil {
		return "offline"
	}
	return fmt.Sprintf("llm:%s:budget=%d:learn=%v:enrich=%v:critic=%v:raise=%v", cfg.Provider.Name(), cfg.LLMBudget, cfg.LearnProfile, cfg.Enrich, cfg.Critic, cfg.RaiseConfidence)
}

// vcsRevisionKey / vcsModifiedKey 构建信息中的 VCS 设置键。
const (
	vcsRevisionKey = "vcs.revision"
	vcsModifiedKey = "vcs.modified"
)

// engineBuildKey 引擎构建身份（计入 memo 指纹）：分析逻辑变了缓存必须失效。
// 干净的 VCS 构建用「版本 + 提交号」（零成本）；本地脏构建/无 VCS 信息时退回可执行文件身份。
func engineBuildKey() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		var rev string
		dirty := true
		for _, s := range bi.Settings {
			switch s.Key {
			case vcsRevisionKey:
				rev = s.Value
			case vcsModifiedKey:
				dirty = s.Value == "true"
			}
		}
		if rev != "" && !dirty {
			return Version + "+" + rev
		}
	}
	// 脏构建：可执行文件的路径 + 大小 + 修改时间足以区分每次重新构建，免去整文件哈希（~30MB）。
	exe, err := os.Executable()
	if err != nil {
		return Version
	}
	info, err := os.Stat(exe)
	if err != nil {
		return Version
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d", exe, info.Size(), info.ModTime().UnixNano())))
	return Version + "+dev-" + hex.EncodeToString(sum[:])
}
