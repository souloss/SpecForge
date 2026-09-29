package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/frontend"
	"github.com/specforge/specforge/internal/infer"
	"github.com/specforge/specforge/internal/schema"
	"github.com/specforge/specforge/internal/verify"
)

// ---- 证据体积与置信度口径 ---------------------------------------------------

// maxSliceBytes 单个 operation 注入 prompt 的切片源码总量上限（约 6k token），
// 超出部分由档位3 工具按需读取。
const maxSliceBytes = 24000

// maxSnippetBytes 单个函数源码片段上限（超长函数截断，保留开头的签名与主干）。
const maxSnippetBytes = 6000

// llmShapePrefix LLM 补出的形状 schema 的类型 ID 前缀（编译器取末段作组件名）。
const llmShapePrefix = "llm/shape/"

// rootDataName 成功响应 data 本身为 any 时，补出形状的组件名主干。
const rootDataName = "Data"

// truncatedMark 源码截断标记（提示模型可用 read_source 工具取剩余部分）。
const truncatedMark = "\n// …(truncated; use read_source for the rest)"

// ---- 阶段上下文 -------------------------------------------------------------

// llmPhase LLM 兜底阶段的共享只读上下文（并发 worker 共享，须只读或自带锁）。
type llmPhase struct {
	prog        frontend.Program              // 程序视图（切片源码、工具、复核）
	catalog     map[string]frontend.ErrorCode // 错误码目录
	p           infer.Provider
	system      string           // 档位2/3 共享 system prompt（规则 + 排序后的错误码目录）
	tools       []infer.ToolSpec // 档位3 工具集（provider 不支持时自动退回档位2）
	log         *slog.Logger
	successCode int                       // 信封成功码
	schemas     map[string]*schema.Schema // 静态 schema 事实（类型 ID → schema）：形状缺口的基准
}

// gapItem 待兜底的 operation：contract 事实 + 载荷副本 + 路由 + 预先组装好的任务。
type gapItem struct {
	f       *facts.Fact           // contract 事实（主协程按序写回）
	cp      facts.ContractPayload // 载荷快照（worker 在其副本上修改）
	handler string                // handler 符号 ID（取调用图切片）
	sl      sliceEvidence         // 调用图切片证据（注入 prompt + 复核）
	targets []shapeTarget         // 形状缺口的写回目标（与 task.ShapeGaps 一一对应）
	task    infer.GapTask         // 发给模型的任务
	tools   []infer.ToolSpec      // 档位3 工具集（nil = 档位2）
}

// gapOutcome 一个 operation 兜底的结果（worker 产出，主协程按序写回）。
type gapOutcome struct {
	cp       facts.ContractPayload // 写回后的载荷
	evidence []facts.Evidence      // 本次 LLM 事实的代码证据（追加到 contract 事实）
	schemas  []*facts.Fact         // 形状缺口补出的 schema 事实
	minConf  float64               // 采纳事实中最低的置信度上限（按核对强度）
	accepted int                   // 通过复核被采纳的事实数
	rejected int                   // 未通过复核被丢弃的事实数
	err      error                 // 调用失败（降级保持 unknown）
}

// llmStats LLM 兜底阶段统计（观测与退出码判定）。
type llmStats struct {
	Attempted     int `json:"attempted"`      // 发起兜底的 operation 数
	Failed        int `json:"failed"`         // 调用失败数（网络/超时/输出不合法）
	BudgetSkipped int `json:"budget_skipped"` // 因预算上限未处理的缺口 operation 数
	Accepted      int `json:"accepted"`       // 采纳的 LLM 事实数
	Rejected      int `json:"rejected"`       // 复核拒绝的 LLM 事实数（幻觉拦截）
	Enriched      int `json:"enriched"`       // 语义增强补出的 summary 数
	// Wrappers L2 包装器摘要统计（前端内执行）。
	Wrappers frontend.StepStats `json:"wrappers"`
	// Adapter L3 适配器生成统计（前端内执行）。
	Adapter frontend.StepStats `json:"adapter"`
	// Routes L4 通用前端路由发现统计（前端内执行）。
	Routes frontend.StepStats `json:"routes"`
	// Contracts L4 通用前端契约抽取统计（前端内执行）。
	Contracts frontend.StepStats `json:"contracts"`
}

// newLLMPhase 构造 LLM 阶段上下文（错误码目录进 system prompt 的稳定前缀）。
func newLLMPhase(prog frontend.Program, catalog map[string]frontend.ErrorCode, successCode int,
	p infer.Provider, log *slog.Logger) *llmPhase {
	return &llmPhase{
		prog: prog, catalog: catalog, p: p, log: log,
		system:      infer.GapSystemPrompt(),
		tools:       frontend.Tools(prog),
		successCode: successCode,
	}
}

// ---- 兜底主流程 -------------------------------------------------------------

// resolveGaps 对带缺口的 operation 做档位2/3 兜底采集（受控并发、确定性写回）。
//
// 预算按收益排序分配（候选证据越强越优先）；ctx 取消后不再派发新任务。
// 每个 operation 独立容错：失败保持 unknown，绝不阻断整体。
// 返回新增的 schema 事实与统计。
func (lp *llmPhase) resolveGaps(ctx context.Context, factList []*facts.Fact, handlers map[string]string,
	budget, concurrency int) ([]*facts.Fact, llmStats) {

	lp.schemas = map[string]*schema.Schema{}
	for _, f := range factList {
		if sc, ok := f.Schema(); ok {
			lp.schemas[strings.TrimPrefix(f.ID, "schema:")] = sc
		}
	}
	var items []gapItem
	for _, f := range factList {
		if f.Kind != facts.KindContract {
			continue
		}
		cp, ok := f.Contract()
		if !ok || !lp.closable(cp) {
			continue
		}
		items = append(items, gapItem{f: f, cp: cp, handler: handlers[strings.TrimPrefix(f.ID, "contract:")]})
	}
	sort.SliceStable(items, func(i, j int) bool { return gapPriority(items[i].cp) < gapPriority(items[j].cp) })
	var st llmStats
	items, st.BudgetSkipped = lp.selectWithinBudget(ctx, items, budget)
	st.Attempted = len(items)
	lp.log.Info("LLM 兜底开始", "operations", len(items), "budget_skipped", st.BudgetSkipped, "concurrency", concurrency)

	outcomes := make([]gapOutcome, len(items))
	var done atomic.Int64 // 已完成数（并发 worker 共享，进度日志用）
	runPool(ctx, len(items), concurrency, func(i int) {
		it := items[i]
		method, path := opMethodPath(it.f.ID)
		defer func() { // worker 内 panic 只让本 operation 降级，不拖垮进程
			if p := recover(); p != nil {
				outcomes[i] = gapOutcome{err: fmt.Errorf("panic: %v", p)}
				lp.log.Error("LLM 兜底 panic，保持 unknown", "op", method+" "+path, "panic", p)
			}
		}()
		lp.log.Debug("LLM 兜底开始", "op", method+" "+path)
		start := time.Now()
		outcomes[i] = lp.resolveOne(ctx, it)
		n := done.Add(1)
		if outcomes[i].err != nil {
			lp.log.Warn("LLM 兜底失败，保持 unknown", "op", method+" "+path, "progress", progress(int(n)-1, len(items)), "err", outcomes[i].err)
			return
		}
		lp.log.Info("LLM 兜底", "progress", progress(int(n)-1, len(items)), "op", method+" "+path,
			"accepted", outcomes[i].accepted, "rejected", outcomes[i].rejected, "ms", time.Since(start).Milliseconds())
	})

	var newFacts []*facts.Fact
	for i, it := range items {
		o := outcomes[i]
		if o.err != nil {
			st.Failed++
			continue
		}
		st.Accepted += o.accepted
		st.Rejected += o.rejected
		if o.accepted == 0 {
			continue // 没有通过复核的事实：载荷保持原样
		}
		it.f.Value = o.cp
		it.f.Evidence = append(it.f.Evidence, o.evidence...)
		it.f.Confidence = recomputeConfidence(it.f.Confidence, o.cp, o.minConf)
		newFacts = append(newFacts, o.schemas...)
	}
	lp.log.Info("LLM 兜底完成", "accepted", st.Accepted, "rejected", st.Rejected, "failed", st.Failed)
	return newFacts, st
}

// prepare 组装单个 operation 的切片证据与任务（纯本地计算，确定性）。
func (lp *llmPhase) prepare(it *gapItem) {
	sites := dynamicSites(it.cp)
	it.sl = lp.slice(it.handler, sites)
	method, path := opMethodPath(it.f.ID)
	it.task = infer.GapTask{
		Method: method, Path: path, Gaps: it.cp.Gaps,
		Sources: it.sl.snippets, DynamicErrorSites: sites,
	}
	for _, c := range it.cp.ErrCandidates {
		it.task.ErrCandidates = append(it.task.ErrCandidates, infer.ErrCandidateItem{Symbol: c.Symbol, Name: c.Name, Code: c.Code})
	}
	for _, c := range it.cp.DataCandidates {
		it.task.DataCandidates = append(it.task.DataCandidates, infer.DataCandidateItem{Name: c.Name})
	}
	if hasAnyGap(it.cp.Gaps, facts.GapErrorPrefix) {
		it.task.ErrorCatalog = lp.catalogSubset(it.sl)
	}
	it.targets = lp.shapeTargets(it.cp)
	for _, t := range it.targets {
		it.task.ShapeGaps = append(it.task.ShapeGaps, t.gap)
	}
	if len(it.targets) > 0 || it.sl.truncated {
		it.tools = lp.tools // 证据放不下或需读结构时开放工具（档位3）
	}
}

// selectWithinBudget 按优先级顺序组装任务并分配预算：缓存可直接满足的 operation 不占预算
// （预算只约束「需付费」的新调用）；顺序执行，选择结果确定。返回入选项与被预算跳过的数量。
func (lp *llmPhase) selectWithinBudget(ctx context.Context, items []gapItem, budget int) ([]gapItem, int) {
	cp, cacheable := lp.p.(*infer.CachedProvider)
	var selected []gapItem
	paid, skipped := 0, 0
	for i := range items {
		lp.prepare(&items[i])
		free := false
		if cacheable {
			if req, err := infer.GapRequest(lp.system, items[i].task, items[i].tools); err == nil {
				free = cp.Cached(ctx, req)
			}
		}
		switch {
		case free:
		case budget <= 0 || paid < budget:
			paid++
		default:
			skipped++
			continue
		}
		selected = append(selected, items[i])
	}
	return selected, skipped
}

// resolveOne 单个 operation：调用 → 在代码上复核产出 → 写回载荷副本。
func (lp *llmPhase) resolveOne(ctx context.Context, it gapItem) gapOutcome {
	if ctxDone(ctx) {
		return gapOutcome{err: ctx.Err()}
	}
	res, err := infer.ResolveGaps(ctx, lp.p, lp.system, it.task, it.tools)
	if err != nil {
		return gapOutcome{err: err}
	}
	out := gapOutcome{cp: cloneContract(it.cp), minConf: 1}
	lp.applyErrorCodes(&out, res, it.sl)
	lp.applyShapes(&out, res.Shapes, it)
	return out
}

// runPool 以 concurrency 个 worker 执行 n 个任务（<=1 串行）；ctx 取消后不再派发新任务。
func runPool(ctx context.Context, n, concurrency int, work func(i int)) {
	if concurrency < 1 {
		concurrency = 1
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < concurrency && w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				work(i)
			}
		}()
	}
	for i := 0; i < n && !ctxDone(ctx); i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}

// ctxDone ctx 是否已取消。
func ctxDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// progress 「i+1/n」进度串。
func progress(i, n int) string {
	return strconv.Itoa(i+1) + "/" + strconv.Itoa(n)
}

// gapPriority 兜底收益优先级（小者先）：any 缺口有字段候选 > 错误码候选已收窄 > 其余。
func gapPriority(cp facts.ContractPayload) int {
	// errCandidateNarrowLimit 错误码候选超过该数视为「未收窄」。
	const errCandidateNarrowLimit = 8
	switch {
	case hasAnyGap(cp.Gaps, facts.GapAnyMarker) && len(cp.DataCandidates) > 0:
		return 0
	case len(cp.ErrCandidates) > 0 && len(cp.ErrCandidates) <= errCandidateNarrowLimit:
		return 1
	}
	return 2
}

// closable 是否有 LLM 协议能闭合的缺口：错误码未解析，或成功响应体上的形状缺口。
// 只剩协议外缺口（请求体 any、未识别包装器——后者由前端 L2 处理）的 operation 不发送。
func (lp *llmPhase) closable(cp facts.ContractPayload) bool {
	return hasAnyGap(cp.Gaps, facts.GapErrorPrefix) || len(lp.shapeTargets(cp)) > 0
}

// hasAnyGap 判断缺口清单里是否含指定子串。
func hasAnyGap(gaps []string, substr string) bool {
	for _, g := range gaps {
		if strings.Contains(g, substr) {
			return true
		}
	}
	return false
}

// dynamicSites 未解析错误行的不可反推来源点（ErrSource 逗号分隔）。
func dynamicSites(cp facts.ContractPayload) []string {
	var out []string
	for _, r := range cp.Responses {
		if r.Envelope == nil || r.Envelope.Code != facts.UnresolvedCode || r.ErrSource == "" {
			continue
		}
		for _, s := range strings.Split(r.ErrSource, ", ") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// cloneContract 载荷深拷贝响应行（worker 在副本上改，避免与主协程共享切片底层数组）。
func cloneContract(cp facts.ContractPayload) facts.ContractPayload {
	cp.Responses = append([]facts.ResponseFact(nil), cp.Responses...)
	cp.Gaps = append([]string(nil), cp.Gaps...)
	return cp
}

// recomputeConfidence LLM 补齐后按剩余缺口重算置信度：缺口清空时取采纳事实的置信度上限，否则不升。
func recomputeConfidence(old float64, cp facts.ContractPayload, cap float64) float64 {
	if len(cp.Gaps) == 0 && old < cap {
		return cap
	}
	return old
}

// ---- 切片证据 ---------------------------------------------------------------

// sliceEvidence 一个 operation 的调用图切片：注入 prompt 的片段 + 用于复核的全量可达函数。
type sliceEvidence struct {
	snippets  []infer.SourceSnippet // 注入 prompt 的源码片段（受体积上限约束）
	truncated bool                  // 是否因体积上限丢弃/截断了片段
	reach     []string              // 全部可达且有函数体的函数 ID（有序，复核用）
}

// slice 组装 handler 的切片证据：handler 优先，其次是含动态错误来源点的函数，再按调用深度。
func (lp *llmPhase) slice(handler string, focus []string) sliceEvidence {
	depth := lp.prog.Reach(handler)
	focusSet := map[string]bool{} // "basename:line"
	for _, s := range focus {
		if i := strings.LastIndex(s, "@"); i >= 0 {
			focusSet[s[i+1:]] = true
		}
	}
	type cand struct {
		id          string
		file        string
		start, end  int
		text        string
		prio, depth int
	}
	var cands []cand
	for fn, d := range depth {
		file, start, end, text, ok := lp.prog.FuncSource(fn)
		if !ok {
			continue
		}
		prio := 2
		if d == 0 {
			prio = 0
		} else if containsFocus(focusSet, file, start, end) {
			prio = 1
		}
		cands = append(cands, cand{fn, file, start, end, text, prio, d})
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.prio != b.prio {
			return a.prio < b.prio
		}
		if a.depth != b.depth {
			return a.depth < b.depth
		}
		return a.id < b.id
	})
	var ev sliceEvidence
	used := 0
	for _, c := range cands {
		ev.reach = append(ev.reach, c.id)
		text := c.text
		if len(text) > maxSnippetBytes {
			text, ev.truncated = text[:maxSnippetBytes]+truncatedMark, true
		}
		if used+len(text) > maxSliceBytes {
			ev.truncated = true
			continue
		}
		used += len(text)
		ev.snippets = append(ev.snippets, infer.SourceSnippet{
			Symbol: c.id, File: lp.prog.RelPath(c.file), Line: c.start, Code: text,
		})
	}
	sort.Strings(ev.reach)
	return ev
}

// containsFocus 函数 [start,end] 是否包含某个聚焦点（聚焦点只有文件基名 + 行号）。
func containsFocus(focus map[string]bool, file string, start, end int) bool {
	base := filepath.Base(file)
	for l := start; l <= end; l++ {
		if focus[base+":"+strconv.Itoa(l)] {
			return true
		}
	}
	return false
}

// catalogSubset 错误码目录中常量名出现在切片可达源码里的条目（有序）。
func (lp *llmPhase) catalogSubset(sl sliceEvidence) []infer.ErrorCodeItem {
	idents := lp.sliceIdents(sl)
	var out []infer.ErrorCodeItem
	for id, e := range lp.catalog {
		if idents[lastSeg(id)] {
			out = append(out, infer.ErrorCodeItem{Symbol: id, Code: e.Code, Msg: e.Msg})
		}
	}
	return infer.SortCatalog(out)
}

// sliceIdents 切片全量可达函数源码中出现的全部标识符。
func (lp *llmPhase) sliceIdents(sl sliceEvidence) map[string]bool {
	set := map[string]bool{}
	for _, fn := range sl.reach {
		_, _, _, text, ok := lp.prog.FuncSource(fn)
		if !ok {
			continue
		}
		start := -1
		for i, r := range text + " " {
			if isIdentRune(r) {
				if start < 0 {
					start = i
				}
				continue
			}
			if start >= 0 {
				set[text[start:i]] = true
				start = -1
			}
		}
	}
	return set
}

// findIdent 在切片全量可达函数源码里找标识符 name 的首次出现（按函数 ID 有序，结果确定）。
func (lp *llmPhase) findIdent(sl sliceEvidence, name string) (facts.Evidence, bool) {
	return lp.findInSlice(sl, func(line string) bool { return containsWord(line, name) })
}

// findStringLit 在切片源码里找字符串字面量 "name" 的首次出现。
func (lp *llmPhase) findStringLit(sl sliceEvidence, name string) (facts.Evidence, bool) {
	quoted := `"` + name + `"`
	return lp.findInSlice(sl, func(line string) bool { return strings.Contains(line, quoted) })
}

// findInSlice 逐函数逐行匹配，返回首个命中行的证据（含指纹与原文）。
func (lp *llmPhase) findInSlice(sl sliceEvidence, match func(string) bool) (facts.Evidence, bool) {
	for _, fn := range sl.reach {
		file, start, end, _, ok := lp.prog.FuncSource(fn)
		if !ok {
			continue
		}
		lines := lp.prog.FileLines(file)
		for l := start; l <= end && l <= len(lines); l++ {
			if match(lines[l-1]) {
				return facts.Evidence{File: file, StartLine: l, EndLine: l,
					BlobSHA: lp.prog.FileHash(file), Quote: strings.TrimSpace(lines[l-1])}, true
			}
		}
	}
	return facts.Evidence{}, false
}

// containsWord line 中是否出现完整标识符 w（两侧不是标识符字符）。
func containsWord(line, w string) bool {
	for i := 0; ; {
		k := strings.Index(line[i:], w)
		if k < 0 {
			return false
		}
		s, e := i+k, i+k+len(w)
		if (s == 0 || !isIdentRune(rune(line[s-1]))) && (e == len(line) || !isIdentRune(rune(line[e]))) {
			return true
		}
		i = s + 1
	}
}

// isIdentRune 是否为 Go 标识符字符。
func isIdentRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// ---- 产出复核与写回 ---------------------------------------------------------

// applyErrorCodes 错误码写回：每个 codeRef 须①命中目录（全名或无歧义末段名）②常量名在切片源码中出现
// （行级证据）；通过者展开为独立错误行（Source=llm）。声明 exhaustive 且无一拒绝时移除「未解析」行。
func (lp *llmPhase) applyErrorCodes(out *gapOutcome, r *infer.GapResolution, sl sliceEvidence) {
	unresolved := -1
	for i, row := range out.cp.Responses {
		if row.Envelope != nil && row.Envelope.Code == facts.UnresolvedCode {
			unresolved = i
			break
		}
	}
	if unresolved < 0 || len(r.ErrorCodes) == 0 {
		return
	}
	base := out.cp.Responses[unresolved]
	present := map[string]bool{}
	for _, row := range out.cp.Responses {
		if row.Envelope != nil && row.Envelope.CodeRef != "" {
			present[row.Envelope.CodeRef] = true
		}
	}
	rejected := 0
	for _, ec := range r.ErrorCodes {
		sym, entry, ok := lp.lookupCode(ec.CodeRef)
		if !ok {
			rejected++ // 幻觉 codeRef（目录外或末段名有歧义）
			continue
		}
		ev, found := lp.findIdent(sl, lastSeg(sym))
		if !found {
			rejected++ // 目录内但本 operation 切片里从未出现：无证据不成事实
			continue
		}
		if present[sym] {
			continue // 静态已定的行不重复
		}
		present[sym] = true
		ev.Quote = "llm:error-code " + lastSeg(sym) + " ← " + ev.Quote
		out.evidence = append(out.evidence, ev)
		out.cp.Responses = append(out.cp.Responses, facts.ResponseFact{
			Status: base.Status, Source: string(facts.SourceLLM),
			Envelope:     &facts.Envelope{Code: entry.Code, CodeRef: sym, Msg: entry.Msg},
			Sink:         filepath.Base(ev.File) + ":" + strconv.Itoa(ev.StartLine),
			EnvelopeType: base.EnvelopeType,
		})
		out.accepted++
		out.minConf = min(out.minConf, facts.VerificationCap(facts.VerifyTyped))
	}
	out.rejected += rejected
	if r.Exhaustive && rejected == 0 && out.accepted > 0 {
		out.cp.Responses = append(out.cp.Responses[:unresolved], out.cp.Responses[unresolved+1:]...)
		out.cp.Gaps = dropGaps(out.cp.Gaps, facts.GapErrorPrefix)
	}
	facts.SortResponses(out.cp.Responses)
}

// lookupCode codeRef → (全限定符号, 目录条目)。末段名在目录中有歧义时拒绝（不猜包）。
func (lp *llmPhase) lookupCode(ref string) (string, frontend.ErrorCode, bool) {
	if e, ok := lp.catalog[ref]; ok {
		return ref, e, true
	}
	short := lastSeg(ref)
	var sym string
	var entry frontend.ErrorCode
	n := 0
	for id, e := range lp.catalog {
		if lastSeg(id) == short {
			sym, entry, n = id, e, n+1
		}
	}
	return sym, entry, n == 1
}

// dropGaps 去掉以 prefix 开头的缺口描述。
func dropGaps(gaps []string, prefix string) []string {
	var out []string
	for _, g := range gaps {
		if !strings.HasPrefix(g, prefix) {
			out = append(out, g)
		}
	}
	return out
}

// shapeTarget 一个形状缺口及其写回位置。
type shapeTarget struct {
	gap       infer.ShapeGap // 发给模型的缺口
	gapText   string         // 对应的缺口描述（闭合后从 cp.Gaps 删除）
	row       int            // 成功行下标
	base      string         // 基准 schema 的类型 ID（空 = data 本身为 any）
	arrayElem bool           // 成功行是 []base（替换元素 schema）
}

// shapeTargets 可交给 LLM 的形状缺口：成功 data 本身为 any，或成功行数据 schema 上某路径为 any。
// 缺口 ID 按出现顺序编号（s0, s1, ...），结果确定。
func (lp *llmPhase) shapeTargets(cp facts.ContractPayload) []shapeTarget {
	var out []shapeTarget
	add := func(t shapeTarget) {
		t.gap.ID = "s" + strconv.Itoa(len(out))
		out = append(out, t)
	}
	for _, g := range cp.Gaps {
		if g == facts.GapSuccessAny {
			for i, r := range cp.Responses {
				if r.Envelope != nil && r.Envelope.Code == lp.successCode && r.SchemaType == "" && !r.Failure {
					add(shapeTarget{gap: infer.ShapeGap{}, gapText: g, row: i})
					break
				}
			}
			continue
		}
		id, path, ok := facts.ParseSchemaAnyGap(g)
		if !ok || lp.schemas[id] == nil {
			continue
		}
		for i, r := range cp.Responses {
			if r.SchemaType == id || r.SchemaType == "[]"+id {
				if path == facts.GapRootPath {
					path = ""
				}
				add(shapeTarget{gap: infer.ShapeGap{Schema: id, Path: path}, gapText: g, row: i, base: id,
					arrayElem: r.SchemaType != id})
				break
			}
		}
	}
	return out
}

// applyShapes 形状缺口写回：每棵形状树逐节点复核（字段名须在切片源码中出现、类型封闭、深度受限），
// 不合规整棵拒绝；通过者沿路径替换出一份 operation 专属 schema（核对强度 text）。
func (lp *llmPhase) applyShapes(out *gapOutcome, shapes []infer.ResolvedShape, it gapItem) {
	if len(shapes) == 0 || len(it.targets) == 0 {
		return
	}
	byID := map[string]shapeTarget{}
	for _, t := range it.targets {
		byID[t.gap.ID] = t
	}
	text, tokens := lp.sliceText(it.sl)
	type rowState struct {
		sc   *schema.Schema
		base string
		arr  bool
		evs  []facts.Evidence
	}
	rows := map[int]*rowState{}
	var order []int
	for _, sh := range shapes {
		t, ok := byID[sh.Gap]
		if !ok {
			out.rejected++
			continue
		}
		node, evs, err := lp.shapeSchema(sh.Shape, text, tokens, it.sl)
		if err != nil {
			out.rejected++
			continue
		}
		st := rows[t.row]
		if st == nil {
			st = &rowState{sc: lp.schemas[t.base], base: t.base, arr: t.arrayElem}
			rows[t.row] = st
			order = append(order, t.row)
		}
		replaced, ok := replaceAt(st.sc, t.gap.Path, node)
		if !ok {
			out.rejected++
			continue
		}
		st.sc, st.evs = replaced, append(st.evs, evs...)
		out.cp.Gaps = dropGaps(out.cp.Gaps, t.gapText)
		out.accepted++
		out.minConf = min(out.minConf, facts.VerificationCap(facts.VerifyText))
	}
	sort.Ints(order)
	for _, ri := range order {
		st := rows[ri]
		name := rootDataName
		if st.base != "" {
			name = lastSeg(st.base)
		}
		op := out.cp.OperationID
		if op != "" {
			op = strings.ToUpper(op[:1]) + op[1:]
		}
		id := llmShapePrefix + shapeHash(st.sc) + "/" + op + "." + name + "For" + op
		row := &out.cp.Responses[ri]
		row.SchemaType, row.Source, row.HasBody = id, string(facts.SourceLLM), true
		if st.arr {
			row.SchemaType = "[]" + id
		}
		for i := range st.evs {
			st.evs[i].File = lp.prog.AbsPath(st.evs[i].File)
		}
		out.schemas = append(out.schemas, &facts.Fact{
			ID: "schema:" + id, Kind: facts.KindSchema, Value: facts.SchemaPayload{Schema: st.sc},
			Source: facts.SourceLLM, Confidence: facts.VerificationCap(facts.VerifyText), Verification: facts.VerifyText,
			Evidence: st.evs, Status: "verified",
		})
	}
}

// shapeSchema 形状树 → schema（verify.Shape 做语言无关核对），并为每个字段名在切片中定位证据行。
func (lp *llmPhase) shapeSchema(n *infer.ShapeNode, text string, tokens map[string]bool, sl sliceEvidence) (*schema.Schema, []facts.Evidence, error) {
	sc, names, err := verify.Shape(n, text, tokens)
	if err != nil {
		return nil, nil, err
	}
	var evs []facts.Evidence
	for _, name := range names {
		ev, ok := lp.findStringLit(sl, name)
		if !ok {
			ev, ok = lp.findIdent(sl, name)
		}
		if ok {
			ev.Quote = "llm:shape-field " + name + " ← " + ev.Quote
			evs = append(evs, ev)
		}
	}
	return sc, evs, nil
}

// replaceAt 在 base 的 path 处替换为 node，返回新 schema（沿路径浅克隆，原 schema 不变）。
// path 语法与缺口一致：a.b 为字段、x[] 为数组元素；空路径替换根。
func replaceAt(base *schema.Schema, path string, node *schema.Schema) (*schema.Schema, bool) {
	if path == "" {
		return node, true
	}
	if base == nil {
		return nil, false
	}
	seg, rest, _ := strings.Cut(path, ".")
	name := strings.TrimRight(seg, "[]")
	arrays := (len(seg) - len(name)) / len("[]")
	cp := *base
	cp.Props = append([]schema.Prop(nil), base.Props...)
	for i, p := range cp.Props {
		if p.Name != name {
			continue
		}
		child := p.Schema
		var stack []*schema.Schema
		for k := 0; k < arrays; k++ {
			if child == nil || child.Items == nil {
				return nil, false
			}
			stack = append(stack, child)
			child = child.Items
		}
		repl, ok := replaceAt(child, rest, node)
		if !ok {
			return nil, false
		}
		for k := len(stack) - 1; k >= 0; k-- {
			wrap := *stack[k]
			wrap.Items = repl
			repl = &wrap
		}
		cp.Props[i].Schema = repl
		return &cp, true
	}
	return nil, false
}

// shapeHashLen 形状 schema 指纹的十六进制长度。
const shapeHashLen = 8

// shapeHash schema 的形状指纹（JSON 序列化后哈希，结果确定）。
func shapeHash(sc *schema.Schema) string {
	b, _ := json.Marshal(sc)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:shapeHashLen]
}

// sliceText 切片全量可达函数的源码文本及其单词集合（标识符、字符串与 tag 中的单词）。
func (lp *llmPhase) sliceText(sl sliceEvidence) (string, map[string]bool) {
	var b strings.Builder
	for _, fn := range sl.reach {
		if _, _, _, text, ok := lp.prog.FuncSource(fn); ok {
			b.WriteString(text)
			b.WriteByte('\n')
		}
	}
	text := b.String()
	return text, verify.Tokens(text)
}

// ---- 档位3 工具 -------------------------------------------------------------

// ---- 画像学习与语义增强 -------------------------------------------------------

// enrichOperations 语义增强（F8）：对无 godoc summary 的 operation 用 handler 源码生成描述。
// 只产出描述性事实（KindEnrichment），证据为 handler 源码位置；失败静默跳过。
func (lp *llmPhase) enrichOperations(ctx context.Context, factList []*facts.Fact, handlers map[string]string,
	concurrency int) []*facts.Fact {
	has := map[string]bool{}
	for _, f := range factList {
		if f.Kind == facts.KindEnrichment {
			has[strings.TrimSuffix(f.ID, ":enrich")] = true
		}
	}
	var keys []string
	for k := range handlers {
		if !has[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]*facts.Fact, len(keys))
	runPool(ctx, len(keys), concurrency, func(i int) {
		defer func() {
			if p := recover(); p != nil {
				lp.log.Error("语义增强 panic，跳过", "op", keys[i], "panic", p)
			}
		}()
		method, path := opMethodPath(keys[i])
		sl := lp.slice(handlers[keys[i]], nil)
		if len(sl.snippets) == 0 {
			return
		}
		h := sl.snippets[0]
		en, err := infer.EnrichOperation(ctx, lp.p, method+" "+path, h.Code)
		if err != nil {
			lp.log.Debug("语义增强失败", "op", keys[i], "err", err)
			return
		}
		out[i] = &facts.Fact{
			ID: keys[i] + ":enrich", Kind: facts.KindEnrichment,
			Value:  facts.EnrichmentPayload{Summary: en.Summary, Description: en.Description},
			Source: facts.SourceLLM, Confidence: facts.VerificationCap(facts.VerifyText), Verification: facts.VerifyText,
			Evidence: []facts.Evidence{{File: lp.prog.AbsPath(h.File), StartLine: h.Line, EndLine: h.Line,
				BlobSHA: lp.prog.FileHash(h.File), Quote: "llm:enrichment"}},
			Status: "verified",
		}
	})
	var res []*facts.Fact
	for _, f := range out {
		if f != nil {
			res = append(res, f)
		}
	}
	return res
}

// lastSeg 符号 ID 的末段（pkg.ErrX → ErrX）。
func lastSeg(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}
