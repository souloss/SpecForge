package generic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/frontend"
	"github.com/specforge/specforge/internal/infer"
	"github.com/specforge/specforge/internal/schema"
	"github.com/specforge/specforge/internal/verify"
)

// Name 通用前端标识（--frontend 取值、memo 指纹、产物 x-framework）。
const Name = "generic"

// frameworkLabel 产物中的框架标注（路由与契约全部来自 LLM + 文本核对）。
const frameworkLabel = "generic-llm"

// ErrNeedsLLM 通用前端离线无法工作（它的全部结论来自 LLM）。
var ErrNeedsLLM = errors.New("the generic frontend discovers routes with an LLM: run with --llm")

// maxRouteFiles 送去做路由发现的候选文件上限（按路径有序截断，控制成本）。
const maxRouteFiles = 200

// maxPromptLines 单个文件送给 LLM 的最大行数（超出部分截断；路由注册通常集中在文件前部）。
const maxPromptLines = 1500

// maxOperations 单次运行抽取契约的 operation 上限（控制成本）。
const maxOperations = 300

// defaultConcurrency 请求未指定并发数时的 LLM 并发调用数。
const defaultConcurrency = 4

// verifyWindow 路由核对窗口：注册行上下各几行内须出现路径字面量与 handler 名（多行注册语句、注解在上一行）。
const verifyWindow = 2

// maxSliceSources 契约抽取任务中 handler 之外附带的被调函数源码数。
const maxSliceSources = 8

// defaultSuccessStatus LLM 未给出任何响应时的兜底成功状态码（并记缺口）。
const defaultSuccessStatus = 200

// jsonMediaType 请求体媒体类型。
const jsonMediaType = "application/json"

// schemaPrefix 通用前端合成 schema 的类型 ID 前缀（编译器取末段作组件名）。
const schemaPrefix = "llm/generic."

// 通用前端的缺口（进入 report，提示人工或静态前端接手）。
const (
	gapContractFailed = "generic frontend: contract extraction failed; only the route is known"
	gapNoResponse     = "generic frontend: no response found in handler source"
	gapBodyRejected   = "generic frontend: request body shape rejected by verification"
	gapRespRejected   = "generic frontend: response body shape rejected by verification"
)

// routeHint 候选文件预筛：以 '/' 开头的字符串字面量（路由路径的共同特征）。
var routeHint = regexp.MustCompile("[\"'`]/[\\w\\-{}:<>*./]*[\"'`]")

// verbHint 候选文件预筛：路由注册常见的方法/注解/装饰器单词（大小写不敏感）。
var verbHint = regexp.MustCompile(`(?i)\b(get|post|put|delete|patch|route|mapping|path|url|api_view|endpoint|handle)\b`)

// stringLiteral 源码行中的字符串字面量（内容为捕获组 1/2/3）。
var stringLiteral = regexp.MustCompile("\"([^\"]*)\"|'([^']*)'|`([^`]*)`")

// pathParam 各框架的路径参数写法：:id、{id}、{id:int}、<id>、<int:id>。
var pathParam = regexp.MustCompile(`:([A-Za-z_]\w*)|\{([A-Za-z_]\w*)(?::[^}]*)?\}|<(?:[A-Za-z_]\w*:)?([A-Za-z_]\w*)>`)

// validMethods 允许的 HTTP 方法。
var validMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "DELETE": true, "PATCH": true, "HEAD": true, "OPTIONS": true, "TRACE": true}

// Frontend L4 通用 LLM 前端：注册在所有静态前端之后，仓库有任意源码文件即匹配。
type Frontend struct{}

// New 构造通用前端。
func New() *Frontend { return &Frontend{} }

// Name 实现 frontend.Frontend。
func (*Frontend) Name() string { return Name }

// Detect 实现 frontend.Frontend：仓库内存在任一源码文件。
func (*Frontend) Detect(repoDir string) bool {
	found := errors.New("found")
	err := filepath.WalkDir(repoDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != repoDir && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if sourceExts[filepath.Ext(path)] {
			return found
		}
		return nil
	})
	return errors.Is(err, found)
}

// IsSource 实现 frontend.Frontend。
func (*Frontend) IsSource(name string) bool { return sourceExts[filepath.Ext(name)] }

// route 一条通过文本核对的路由。
type route struct {
	method  string   // HTTP 方法
	path    string   // OpenAPI 路径（参数已归一为 {name}）
	params  []string // 路径参数名（有序）
	handler string   // handler 符号 ID（文本视图口径）
	name    string   // handler 名（operationId 来源；内联为空）
	file    string   // 注册所在文件（绝对路径）
	line    int      // 注册行号
}

// Analyze 实现 frontend.Frontend：预筛候选文件 → LLM 逐文件发现路由 → 文本核对 → LLM 逐 operation 抽契约 → 核对 → 事实。
func (*Frontend) Analyze(ctx context.Context, req frontend.Request) (*frontend.Analysis, error) {
	if req.Provider == nil {
		return nil, fmt.Errorf("%w: %w", frontend.ErrConfig, ErrNeedsLLM)
	}
	end := req.Stage("scan")
	serviceRoot := req.Manifest.Root
	if serviceRoot == "" {
		serviceRoot = req.ServiceRoot
	}
	if req.Service != "" && serviceRoot == "" {
		return nil, fmt.Errorf("%w: generic service %q requires --service-root", frontend.ErrConfig, req.Service)
	}
	root := req.RepoDir
	if serviceRoot != "" {
		if filepath.IsAbs(serviceRoot) {
			root = serviceRoot
		} else {
			root = filepath.Join(req.RepoDir, serviceRoot)
		}
	}
	if info, statErr := os.Stat(root); statErr != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: service root %q is not a readable directory", frontend.ErrConfig, root)
	}
	prog, err := NewProgram(root)
	if err == nil && len(prog.Files()) == 0 {
		err = fmt.Errorf("no source files found")
	}
	var cands []string
	if err == nil {
		cands = candidateFiles(prog)
	}
	end()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", frontend.ErrConfig, err)
	}
	conc := req.Concurrency
	if conc < 1 {
		conc = defaultConcurrency
	}
	an := &frontend.Analysis{Program: prog, Service: serviceTitle(req), Framework: frameworkLabel, Handlers: map[string]string{}}
	an.Stats.Packages = len(prog.Files())

	end = req.Stage("llm-routes")
	routes, rst := discover(ctx, req, prog, cands, conc)
	end()
	an.Routes = rst
	an.Stats.Routes, an.Stats.RoutesResolved = rst.Attempted, len(routes)
	an.Stats.RoutesUnique = len(routes)
	an.Stats.RoutesCollapsed = max(0, rst.Attempted-rst.Rejected-len(routes))
	req.Log.Info("通用前端路由发现完成", "files", len(cands), "routes", len(routes), "rejected", rst.Rejected, "failed", rst.Failed)

	// recall 守卫：正则扫出「疑似路由注册行」与已采纳路由的差集，报进 MissedRoutes 清单
	// （不擅自进 spec）。这是 route recall 的确定性下界，防 LLM 漏报端点。
	an.MissedRoutes = scanMissedRoutes(prog, cands, routes)

	end = req.Stage("llm-contracts")
	an.Facts, an.Contracts = extract(ctx, req, prog, routes, conc)
	end()
	for _, r := range routes {
		an.Handlers[facts.OpKey(r.method, r.path)] = r.handler
	}
	an.Stats.Symbols = prog.DefCount()
	an.Stats.SinkSites = len(an.Facts)
	return an, nil
}

// serviceTitle 产物标题：显式服务名，否则仓库目录名。
func serviceTitle(req frontend.Request) string {
	if req.Service != "" {
		return req.Service
	}
	abs, err := filepath.Abs(req.RepoDir)
	if err != nil {
		return req.RepoDir
	}
	return filepath.Base(abs)
}

// candidateFiles 预筛：同一行既有以 '/' 开头的字符串字面量、又有路由单词的文件（有序、截断到上限）。
func candidateFiles(prog *Program) []string {
	var out []string
	for _, f := range prog.Files() {
		for _, line := range prog.FileLines(f) {
			if routeHint.MatchString(line) && verbHint.MatchString(line) {
				out = append(out, f)
				break
			}
		}
		if len(out) == maxRouteFiles {
			break
		}
	}
	return out
}

// numbered 带行号的源码（"12| code"），超长截断。
func numbered(lines []string) string {
	var b strings.Builder
	for i, l := range lines {
		if i == maxPromptLines {
			b.WriteString("…(truncated)\n")
			break
		}
		fmt.Fprintf(&b, "%d| %s\n", i+1, l)
	}
	return b.String()
}

// runPool 以 conc 个 worker 执行 n 个任务（结果由调用方按下标写入，顺序与并发无关）。
func runPool(ctx context.Context, n, conc int, work func(i int)) {
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < conc && w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				work(i)
			}
		}()
	}
	for i := 0; i < n && ctx.Err() == nil; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}

// scanMissedRoutes recall 守卫：把候选源码里所有「疑似路由注册行」与已采纳路由的
// (file,line) 集合做差集，产出疑似遗漏清单（不进 spec）。
//
// 启发式：一行同时命中 verbHint（路由动词/装饰器）与 routeHint（以 / 开头的字符串字面量）。
// 已经由 LLM 报出并经 verifyRoute 采纳的注册行不重复报。为避免正则误报污染产物，
// 这里只产出 frontend.RouteIssue 清单，由 report/CLI/CI 消费，不生成 route 事实。
func scanMissedRoutes(prog *Program, files []string, accepted []route) []frontend.RouteIssue {
	seen := map[string]bool{} // "relPath:line" → 已采纳
	for _, r := range accepted {
		seen[fmt.Sprintf("%s:%d", prog.RelPath(r.file), r.line)] = true
	}
	var out []frontend.RouteIssue
	for _, f := range files {
		rel := prog.RelPath(f)
		lines := prog.FileLines(f)
		for i, line := range lines {
			if !verbHint.MatchString(line) {
				continue
			}
			lit := routeHint.FindString(line)
			if lit == "" || seen[fmt.Sprintf("%s:%d", rel, i+1)] {
				continue
			}
			out = append(out, frontend.RouteIssue{
				File:    rel,
				Line:    i + 1,
				RawPath: strings.Trim(lit, `"'`),
				Reason:  "route hint not reported by the LLM route discoverer",
			})
		}
	}
	return out
}

// discover 逐文件路由发现并核对；按 (method, path) 去重（先到先得，文件序确定）。
// 统计口径：Attempted = LLM 报告的路由数，Accepted/Rejected = 核对结果，Failed = 调用失败的文件数。
func discover(ctx context.Context, req frontend.Request, prog *Program, files []string, conc int) ([]route, frontend.StepStats) {
	lists := make([]*infer.RouteList, len(files))
	errs := make([]error, len(files))
	runPool(ctx, len(files), conc, func(i int) {
		lists[i], errs[i] = infer.DiscoverRoutes(ctx, req.Provider,
			infer.RouteTask{File: prog.RelPath(files[i]), Lines: numbered(prog.FileLines(files[i]))})
	})
	var st frontend.StepStats
	var out []route
	seen := map[string]bool{}
	for i, f := range files {
		if errs[i] != nil {
			st.Failed++
			req.Log.Warn("路由发现失败", "file", prog.RelPath(f), "err", errs[i])
			continue
		}
		if lists[i] == nil {
			continue
		}
		for _, dr := range lists[i].Routes {
			st.Attempted++
			r, err := verifyRoute(prog, f, dr)
			if err != nil {
				st.Rejected++
				req.Log.Debug("路由未通过核对", "file", prog.RelPath(f), "route", dr.Method+" "+dr.Path, "err", err)
				continue
			}
			key := facts.OpKey(r.method, r.path)
			if seen[key] {
				continue
			}
			seen[key] = true
			st.Accepted++
			out = append(out, r)
		}
	}
	if len(out) > maxOperations {
		req.Log.Warn("operation 数超过上限，截断", "total", len(out), "limit", maxOperations)
		out = out[:maxOperations]
	}
	return out, st
}

// verifyRoute 文本核对：方法合法；注册行有效；窗口内有字符串字面量是所报路径的后缀（前缀来自挂载/分组，
// 须作为字面量出现在该文件或仓库的其它候选行中）；非内联 handler 的名字出现在窗口内。
func verifyRoute(prog *Program, file string, dr infer.DiscoveredRoute) (route, error) {
	method := strings.ToUpper(strings.TrimSpace(dr.Method))
	if !validMethods[method] {
		return route{}, fmt.Errorf("method %q not allowed", dr.Method)
	}
	if !prog.ValidLine(file, dr.Line) || !strings.HasPrefix(dr.Path, "/") {
		return route{}, fmt.Errorf("line %d or path %q invalid", dr.Line, dr.Path)
	}
	window := prog.SourceRange(file, dr.Line-verifyWindow, dr.Line+verifyWindow)
	lit, ok := pathLiteralIn(window, dr.Path)
	if !ok {
		return route{}, fmt.Errorf("no path literal matching %q near line %d", dr.Path, dr.Line)
	}
	if prefix := strings.TrimSuffix(dr.Path, lit); prefix != "" && prefix != "/" && !prefixLiteralExists(prog, prefix) {
		return route{}, fmt.Errorf("mount prefix %q not found as a literal", prefix)
	}
	r := route{method: method, file: file, line: dr.Line}
	r.path, r.params = normalizePath(dr.Path)
	name := strings.TrimSpace(dr.Handler)
	if name == "" || strings.EqualFold(name, inlineMark) {
		r.handler = prog.InlineID(file, dr.Line)
		return r, nil
	}
	short := name
	if i := strings.LastIndexAny(short, ".:"); i >= 0 {
		short = short[i+1:]
	}
	if !verify.NameInSource(short, window, verify.Tokens(window)) {
		return route{}, fmt.Errorf("handler %q not on the registration lines", name)
	}
	r.name = short
	if d, ok := prog.Resolve(name, file); ok {
		r.handler = d.ID
	} else {
		r.handler = prog.InlineID(file, dr.Line) // 定义不可定位（跨语言/动态注册）：以注册处为区域
	}
	return r, nil
}

// pathLiteralIn 窗口内是否有字符串字面量等于所报路径或为其后缀（按「/」段边界），返回该字面量。
func pathLiteralIn(window, path string) (string, bool) {
	best := ""
	for _, m := range stringLiteral.FindAllStringSubmatch(window, -1) {
		lit := m[1] + m[2] + m[3]
		if lit == "" || len(lit) <= len(best) || !strings.HasSuffix(path, lit) {
			continue
		}
		rest := path[:len(path)-len(lit)]
		if rest == "" || strings.HasSuffix(rest, "/") || strings.HasPrefix(lit, "/") {
			best = lit
		}
	}
	return best, best != ""
}

// prefixLiteralExists 挂载前缀（可能由多级组成）的每一段字面量都能在候选源码中找到。
func prefixLiteralExists(prog *Program, prefix string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	for _, f := range prog.Files() {
		for _, line := range prog.FileLines(f) {
			for _, m := range stringLiteral.FindAllStringSubmatch(line, -1) {
				lit := strings.TrimSuffix(m[1]+m[2]+m[3], "/")
				if lit != "" && strings.HasPrefix(lit, "/") && strings.HasSuffix(prefix, lit) {
					rest := strings.TrimSuffix(prefix, lit)
					if rest == "" || prefixLiteralExists(prog, rest) {
						return true
					}
				}
			}
		}
	}
	return false
}

// normalizePath 各框架路径参数写法 → OpenAPI {name}，返回路径与参数名（有序）。
func normalizePath(p string) (string, []string) {
	var params []string
	out := pathParam.ReplaceAllStringFunc(p, func(m string) string {
		sm := pathParam.FindStringSubmatch(m)
		name := sm[1] + sm[2] + sm[3]
		params = append(params, name)
		return "{" + name + "}"
	})
	if len(out) > 1 {
		out = strings.TrimSuffix(out, "/")
	}
	return out, params
}

// opOutcome 一个 operation 的契约抽取结果（按路由下标写入，顺序确定）。
type opOutcome struct {
	contract *infer.ExtractedContract // LLM 结果（失败为 nil）
	err      error                    // 调用错误
	text     string                   // 切片源码拼接（核对用）
	sources  []infer.SourceSnippet    // 切片（证据定位用）
}

// extract 逐 operation 抽契约并核对，产出 route/contract/schema 事实；统计口径同 discover（按 operation）。
func extract(ctx context.Context, req frontend.Request, prog *Program, routes []route, conc int) ([]*facts.Fact, frontend.StepStats) {
	outs := make([]opOutcome, len(routes))
	tools := frontend.Tools(prog)
	runPool(ctx, len(routes), conc, func(i int) {
		r := routes[i]
		src := sliceOf(prog, r.handler)
		o := opOutcome{sources: src}
		for _, s := range src {
			o.text += s.Code + "\n"
		}
		o.contract, o.err = infer.ExtractContract(ctx, req.Provider, infer.ContractTask{
			Source: "generic", Operation: r.method + " " + r.path,
			AllowedFields: []string{"params", "requestBody", "responses"},
			Method:        r.method, Path: r.path, Sources: src,
		}, tools)
		outs[i] = o
	})
	var st frontend.StepStats
	var fl []*facts.Fact
	for i, r := range routes {
		st.Attempted++
		o := outs[i]
		if o.err != nil {
			st.Failed++
			req.Log.Warn("契约抽取失败，仅保留路由", "op", r.method+" "+r.path, "err", o.err)
		}
		opFacts, rejected := buildFacts(prog, r, o)
		if o.err == nil {
			if rejected > 0 {
				st.Rejected++
			} else {
				st.Accepted++
			}
		}
		fl = append(fl, opFacts...)
	}
	return fl, st
}

// sliceOf handler 区域 + 文本可达的被调函数源码（按深度、ID 有序，截断到上限）。
func sliceOf(prog *Program, handler string) []infer.SourceSnippet {
	reach := prog.Reach(handler)
	ids := make([]string, 0, len(reach))
	for id := range reach {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if reach[ids[i]] != reach[ids[j]] {
			return reach[ids[i]] < reach[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if len(ids) > maxSliceSources+1 {
		ids = ids[:maxSliceSources+1]
	}
	var out []infer.SourceSnippet
	for _, id := range ids {
		if file, start, _, text, ok := prog.FuncSource(id); ok {
			out = append(out, infer.SourceSnippet{Symbol: id, File: prog.RelPath(file), Line: start, Code: text})
		}
	}
	return out
}

// buildFacts 一个 operation 的事实：route（注册行证据）+ contract（参数/请求体/响应逐项核对）+ 合成 schema。
// 返回事实与被核对拒绝的条目数。LLM 事实的置信度按 text 级核对封顶。
func buildFacts(prog *Program, r route, o opOutcome) ([]*facts.Fact, int) {
	conf := facts.VerificationCap(facts.VerifyText)
	opKey := facts.OpKey(r.method, r.path)
	regEv := evidenceAt(prog, r.file, r.line, "route "+r.method+" "+r.path)
	routeFact := llmFact(opKey, facts.RoutePayload{Method: r.method, Path: r.path, Handler: r.handler}, conf, regEv)
	opID := operationID(r)
	cp := facts.ContractPayload{OperationID: opID, Tags: tagsOf(prog, r)}
	evs := []facts.Evidence{regEv}
	if file, start, _, _, ok := prog.FuncSource(r.handler); ok && start != r.line {
		evs = append(evs, evidenceAt(prog, file, start, "handler"))
	}
	var schemaFacts []*facts.Fact
	rejected := 0
	c := o.contract
	if c == nil {
		cp.Gaps = append(cp.Gaps, gapContractFailed)
		c = &infer.ExtractedContract{}
	}
	tokens := verify.Tokens(o.text)

	// 参数：位置/类型取封闭集合，名字须出现在切片源码中；路径参数以路由模板为准。
	pathSeen := map[string]bool{}
	for _, p := range c.Params {
		if !verify.ValidParamIn(p.In) || !verify.NameInSource(p.Name, o.text, tokens) {
			rejected++
			continue
		}
		if p.In == "path" {
			if !contains(r.params, p.Name) || pathSeen[p.Name] {
				continue
			}
			pathSeen[p.Name] = true
		}
		typ := verify.ScalarType(p.Type)
		// 参数级约束核对：enum/pattern 字面量须出现在源码；format 取封闭集合。
		for _, v := range p.Enum {
			if !verify.NameInSource(v, o.text, tokens) {
				rejected++
				continue
			}
		}
		if p.Pattern != "" && !verify.NameInSource(p.Pattern, o.text, tokens) {
			rejected++
			continue
		}
		if p.Format != "" && !verify.ValidFormat(p.Format) {
			rejected++
			continue
		}
		cp.Params = append(cp.Params, facts.ParamFact{
			In: p.In, Name: p.Name, Required: p.Required || p.In == "path", Type: typ,
			Format: p.Format, Enum: p.Enum, Origin: Name + ":llm",
		})
	}
	for _, name := range r.params {
		if !pathSeen[name] {
			cp.Params = append(cp.Params, facts.ParamFact{In: "path", Name: name, Required: true, Type: "string", Origin: Name + ":route"})
		}
	}

	// 请求体。
	if c.RequestBody != nil {
		if sc, ev, err := shapeOf(prog, c.RequestBody, o); err == nil {
			id := schemaPrefix + opID + "Request"
			schemaFacts = append(schemaFacts, llmFact("schema:"+id, facts.SchemaPayload{Schema: sc}, conf, ev...))
			cp.RequestBody = &facts.BodyFact{ContentType: jsonMediaType, SchemaType: id, Origin: Name + ":llm"}
		} else {
			rejected++
			cp.Gaps = append(cp.Gaps, gapBodyRejected)
		}
	}

	// 响应：状态码合法；同状态码的成功/错误体各取首个。
	seen := map[string]bool{}
	for _, resp := range c.Responses {
		key := strconv.Itoa(resp.Status) + strconv.FormatBool(resp.Error)
		if !verify.ValidStatus(resp.Status) || seen[key] {
			rejected++
			continue
		}
		seen[key] = true
		row := facts.ResponseFact{Status: resp.Status, Raw: true, Failure: resp.Error, Sink: Name + ":llm", Source: string(facts.SourceLLM)}
		if resp.Body != nil {
			if sc, ev, err := shapeOf(prog, resp.Body, o); err == nil {
				branch := ""
				if resp.Error {
					branch = "Error"
				}
				id := schemaPrefix + opID + branch + "Response" + strconv.Itoa(resp.Status)
				schemaFacts = append(schemaFacts, llmFact("schema:"+id, facts.SchemaPayload{Schema: sc}, conf, ev...))
				row.SchemaType, row.HasBody = id, true
			} else {
				rejected++
				cp.Gaps = appendUniq(cp.Gaps, gapRespRejected)
			}
		}
		cp.Responses = append(cp.Responses, row)
	}
	if len(cp.Responses) == 0 {
		cp.Gaps = appendUniq(cp.Gaps, gapNoResponse)
		cp.Responses = []facts.ResponseFact{{Status: defaultSuccessStatus, Raw: true, Sink: Name + ":default"}}
	}
	sort.SliceStable(cp.Responses, func(i, j int) bool { return cp.Responses[i].Status < cp.Responses[j].Status })
	contractConf := conf
	if len(cp.Gaps) > 0 {
		contractConf = min(conf, gapConfidence)
	}
	contract := llmFact("contract:"+opKey, cp, contractConf, evs...)
	return append([]*facts.Fact{routeFact, contract}, schemaFacts...), rejected
}

// gapConfidence 带缺口的通用前端 operation 的置信度上限。
const gapConfidence = 0.4

// shapeOf 形状树核对（字段名须出现在切片源码中）→ schema，并为字段名定位证据行（首个出现处）。
func shapeOf(prog *Program, n *infer.ShapeNode, o opOutcome) (*schema.Schema, []facts.Evidence, error) {
	sc, names, err := verify.Shape(n, o.text, verify.Tokens(o.text))
	if err != nil {
		return nil, nil, err
	}
	var ev []facts.Evidence
	for _, name := range names {
		if e, ok := locate(prog, o.sources, name); ok {
			ev = append(ev, e)
		}
	}
	if len(ev) == 0 && len(o.sources) > 0 { // 无字段（标量/空对象）：以 handler 区域为证据
		s := o.sources[0]
		ev = append(ev, evidenceAt(prog, prog.AbsPath(s.File), s.Line, "shape"))
	}
	return sc, ev, nil
}

// locate 名字在切片中首次出现的行 → 证据。
func locate(prog *Program, src []infer.SourceSnippet, name string) (facts.Evidence, bool) {
	for _, s := range src {
		for i, line := range strings.Split(s.Code, "\n") {
			if verify.NameInSource(name, line, verify.Tokens(line)) {
				return evidenceAt(prog, prog.AbsPath(s.File), s.Line+i, "field "+name), true
			}
		}
	}
	return facts.Evidence{}, false
}

// evidenceAt 单行证据（带内容指纹与 "llm:<标签> ← 源码行" 引文）。
func evidenceAt(prog *Program, file string, line int, label string) facts.Evidence {
	return facts.Evidence{File: file, StartLine: line, EndLine: line, BlobSHA: prog.FileHash(file),
		Quote: "llm:" + label + " ← " + strings.TrimSpace(prog.SourceRange(file, line, line))}
}

// llmFact LLM 来源、text 级核对的事实。
func llmFact(id string, p facts.Payload, conf float64, ev ...facts.Evidence) *facts.Fact {
	f := facts.NewFact(id, p, ev...)
	f.Source, f.Confidence, f.Verification = facts.SourceLLM, conf, facts.VerifyText
	return f
}

// operationID handler 名（首字母小写）；内联 handler 用 方法+路径 拼接。
func operationID(r route) string {
	name := r.name
	if name == "" {
		var b strings.Builder
		b.WriteString(strings.ToLower(r.method))
		for _, seg := range strings.Split(r.path, "/") {
			seg = strings.Trim(seg, "{}")
			if seg == "" {
				continue
			}
			b.WriteString(strings.ToUpper(seg[:1]) + seg[1:])
		}
		return componentSafe(b.String())
	}
	return strings.ToLower(name[:1]) + name[1:]
}

// componentSafe 只保留标识符字符（operationId 与组件名要求）。
func componentSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return -1
	}, s)
}

// tagsOf handler 定义所在文件名（去扩展名）作为标签：与静态前端「按 handler 归属分组」口径一致。
func tagsOf(prog *Program, r route) []string {
	file, _, _, _, ok := prog.FuncSource(r.handler)
	if !ok {
		file = r.file
	}
	base := filepath.Base(file)
	return []string{strings.TrimSuffix(base, filepath.Ext(base))}
}

// contains 切片是否含 s。
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// appendUniq 去重追加。
func appendUniq(xs []string, s string) []string {
	if contains(xs, s) {
		return xs
	}
	return append(xs, s)
}
