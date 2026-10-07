package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/specforge/specforge/internal/compiler"
	"github.com/specforge/specforge/internal/contract"
	"github.com/specforge/specforge/internal/memo"
	"github.com/specforge/specforge/internal/openapi"
)

// renderReport 置信度报告（F11）：低置信明细 + 证据闸门丢弃清单 + 隔离失败 + 全量概览。
// 内容只取决于分析结果（无时间戳等易变信息），同输入逐字节一致。
func renderReport(doc *compiler.Document, res *Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# SpecForge 置信度报告\n\n")
	fmt.Fprintf(&b, "- operations: %d, schema types: %d\n", len(doc.Operations), len(doc.Schemas))
	fmt.Fprintf(&b, "- cache: run memo=%v, contract graph snapshot=%v\n", res.Cached, res.GraphCached)
	fmt.Fprintf(&b, "- Contract Graph conflicts: %d, unknown schemas: %d\n", res.Conflicts, res.UnknownSchemas)
	states := map[string]int{}
	sources := map[string]int{}
	for _, operation := range doc.Operations {
		states[operation.State]++
		for _, source := range operation.Sources {
			sources[source]++
		}
	}
	fmt.Fprintf(&b, "- operation states: %s\n", stableCounts(states))
	fmt.Fprintf(&b, "- evidence sources: %s\n", stableCounts(sources))
	if len(res.GraphDiagnostics) > 0 {
		fmt.Fprintf(&b, "- Contract Graph diagnostics: %d\n", len(res.GraphDiagnostics))
		for _, diagnostic := range res.GraphDiagnostics {
			fmt.Fprintf(&b, "  - %s: %s\n", diagnostic.Code, diagnostic.Message)
		}
	}
	fmt.Fprintf(&b, "- 低置信 operation (<%.1f): %d\n", lowConfThreshold, res.LowConf)
	if len(res.Gaps) > 0 {
		fmt.Fprintf(&b, "- 静态缺口 operation: %d（LLM 兜底后仍有缺口: %d）\n", len(res.Gaps), res.RemainingGaps)
	}
	if res.LLM.Attempted > 0 {
		fmt.Fprintf(&b, "- LLM 兜底: %d 个 operation，采纳 %d 条事实，复核拒绝 %d 条，失败 %d，预算跳过 %d\n",
			res.LLM.Attempted, res.LLM.Accepted, res.LLM.Rejected, res.LLM.Failed, res.LLM.BudgetSkipped)
	}
	if a := res.LLM.Adapter; a.Attempted > 0 {
		fmt.Fprintf(&b, "- LLM 适配器生成: 采纳 %d，复核拒绝 %d，失败 %d（声明见 .specforge/adapters/，请审阅后提交）\n", a.Accepted, a.Rejected, a.Failed)
	}
	if w := res.LLM.Wrappers; w.Attempted > 0 {
		fmt.Fprintf(&b, "- LLM 包装器摘要: %d 个，采纳 %d，复核拒绝 %d，失败 %d\n", w.Attempted, w.Accepted, w.Rejected, w.Failed)
	}
	if r := res.LLM.Routes; r.Attempted > 0 || r.Failed > 0 {
		fmt.Fprintf(&b, "- LLM 通用前端路由发现: 报告 %d 条，采纳 %d，文本核对拒绝 %d，失败文件 %d\n", r.Attempted, r.Accepted, r.Rejected, r.Failed)
	}
	if c := res.LLM.Contracts; c.Attempted > 0 {
		fmt.Fprintf(&b, "- LLM 通用前端契约抽取: %d 个 operation，全部通过 %d，部分拒绝 %d，失败 %d（置信度按文本核对封顶）\n", c.Attempted, c.Accepted, c.Rejected, c.Failed)
	}
	if res.LLM.Enriched > 0 {
		fmt.Fprintf(&b, "- LLM 语义增强: %d 个 operation\n", res.LLM.Enriched)
	}
	b.WriteString("\n")

	var low []*compiler.Operation
	for i := range doc.Operations {
		if doc.Operations[i].Confidence < lowConfThreshold {
			low = append(low, &doc.Operations[i])
		}
	}
	sort.SliceStable(low, func(i, j int) bool { return low[i].Confidence < low[j].Confidence })
	if len(low) == 0 {
		fmt.Fprintf(&b, "全部 operation 置信度 ≥ %.1f。\n\n", lowConfThreshold)
	}
	for _, op := range low {
		fmt.Fprintf(&b, "## %s %s (confidence %.2f)\n\n", op.Method, op.Path, op.Confidence)
		for _, u := range op.Unknowns {
			fmt.Fprintf(&b, "- 未解析: %s\n", u)
		}
		for _, e := range op.Evidence {
			fmt.Fprintf(&b, "- 证据: %s\n", e)
		}
		for _, r := range op.Responses {
			if len(r.Sinks) > 0 {
				fmt.Fprintf(&b, "- 响应 %s 写出点: %s\n", r.Status, strings.Join(r.Sinks, ", "))
			}
		}
		b.WriteString("\n")
	}
	if len(res.OpFailures) > 0 {
		fmt.Fprintf(&b, "## 分析失败被隔离的 operation (%d)\n\n", len(res.OpFailures))
		for _, f := range res.OpFailures {
			fmt.Fprintf(&b, "- %s: %s\n", f.Op, f.Error)
		}
		b.WriteString("\n")
	}
	if len(res.MissedRoutes) > 0 {
		fmt.Fprintf(&b, "## 疑似遗漏路由（recall 守卫，未进 spec）(%d)\n\n", len(res.MissedRoutes))
		for _, m := range res.MissedRoutes {
			fmt.Fprintf(&b, "- %s:%d %s (%s)\n", m.File, m.Line, m.RawPath, m.Reason)
		}
		b.WriteString("\n")
	}
	if len(res.RuntimeOrphans) > 0 {
		fmt.Fprintf(&b, "## runtime 观察到但契约未覆盖的 route（%d）\n\n", len(res.RuntimeOrphans))
		for _, key := range res.RuntimeOrphans {
			fmt.Fprintf(&b, "- %s\n", key)
		}
		b.WriteString("\n")
	}
	if len(res.DroppedIDs) > 0 {
		fmt.Fprintf(&b, "## 证据闸门丢弃的事实 (%d)\n\n", len(res.DroppedIDs))
		for _, id := range res.DroppedIDs {
			fmt.Fprintf(&b, "- %s\n", id)
		}
		b.WriteString("\n")
	}
	writeOpOverview(&b, doc)
	return b.String()
}

func stableCounts(values map[string]int) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, values[key]))
	}
	return strings.Join(parts, ", ")
}

// writeOpOverview 全量 operation 概览表：每个接口的置信度、成功响应体与可静态定出的信封码，
// 高置信接口不进明细也能在报告中核对。
func writeOpOverview(b *strings.Builder, doc *compiler.Document) {
	b.WriteString("## 全部 operation 概览\n\n")
	b.WriteString("| Method | Path | 置信度 | 响应体 | 信封码 |\n|---|---|---|---|---|\n")
	for i := range doc.Operations {
		op := &doc.Operations[i]
		body, codes := "-", []string{}
		for _, r := range op.Responses {
			if names := variantBodies(r); len(names) > 0 {
				body = strings.Join(names, variantSep)
			}
			for _, c := range r.Codes {
				codes = append(codes, strconv.Itoa(c))
			}
			if r.HasDynamic {
				codes = append(codes, dynamicCodeMark)
			}
			if r.HasUncoded {
				codes = append(codes, uncodedCodeMark)
			}
			if r.HasUnresolved {
				codes = append(codes, "?")
			}
		}
		fmt.Fprintf(b, "| %s | %s | %.2f | %s | %s |\n", op.Method, op.Path, op.Confidence, body, strings.Join(codes, ","))
	}
}

// 报告信封码列中静态定性错误行的记号：动态码（取自运行时值）/ 不带业务码。
const (
	dynamicCodeMark = "dyn"
	uncodedCodeMark = "none"
)

// SchemaVersion 机器契约版本：operations.json 与 CLI --json 信封共用；字段语义不兼容变更时递增。
const SchemaVersion = "1"

// OperationsFile operations.json 的顶层结构（explain / ops 命令与 agent 消费）。
type OperationsFile struct {
	SchemaVersion string               `json:"schema_version"` // 契约版本
	Operations    []compiler.Operation `json:"operations"`     // 编译后的 operation（稳定排序）
}

// operationsJSON operation 级证据视图。
func operationsJSON(doc *compiler.Document) ([]byte, error) {
	return json.MarshalIndent(OperationsFile{SchemaVersion: SchemaVersion, Operations: doc.Operations}, "", "  ")
}

func graphJSON(graph *contract.Graph) ([]byte, error) {
	return graph.MarshalStable()
}

// ReadOperations 读取 gen 产出的 operations.json（校验契约版本）。
func ReadOperations(outDir string) (*OperationsFile, error) {
	b, err := os.ReadFile(filepath.Join(outDir, operationsFile))
	if err != nil {
		return nil, err
	}
	var f OperationsFile
	if bytes.HasPrefix(bytes.TrimSpace(b), []byte("[")) {
		return nil, fmt.Errorf("%s was written by an older specforge (no schema_version); re-run gen", operationsFile)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s is corrupt: %w", operationsFile, err)
	}
	if f.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%s has schema_version %q, this build reads %q (re-run gen)", operationsFile, f.SchemaVersion, SchemaVersion)
	}
	return &f, nil
}

// memoArtifacts memo 条目包含的产物（缺任一视为未命中）。
var memoArtifacts = []string{specFile, reportFile, operationsFile, contractFile, summaryFile}

// loadMemo 命中时把产物写回 outDir 并返回摘要回填的 Result。
func loadMemo(store *memo.Store, fp, outDir string) (*Result, bool) {
	files, ok := store.Load(fp, memoArtifacts...)
	if !ok {
		return nil, false
	}
	var res Result
	if json.Unmarshal(files[summaryFile], &res) != nil {
		return nil, false
	}
	delete(files, summaryFile)
	doc, err := openapi.LoadBytes(files[specFile], filepath.Join(outDir, specFile))
	if err != nil {
		return nil, false
	}
	valid := doc.Validate()
	doc.Close()
	if len(valid) > 0 {
		return nil, false
	}
	if writeArtifacts(outDir, files) != nil {
		return nil, false
	}
	return &res, true
}

// saveMemo 把本次产物与摘要存入 memo。
func saveMemo(store *memo.Store, fp string, artifacts map[string][]byte, res *Result) error {
	sum, err := json.Marshal(res)
	if err != nil {
		return err
	}
	files := map[string][]byte{summaryFile: sum}
	for k, v := range artifacts {
		files[k] = v
	}
	return store.Save(fp, files)
}

// variantSep 报告中同一状态码多个响应体变体（oneOf）的分隔符。
const variantSep = " \\| "

// variantBodies 响应的业务体展示名：多变体（oneOf）逐个列出，单变体取主字段；无业务体为空。
func variantBodies(r compiler.ResponseOut) []string {
	name := func(schemaName, arrayElem, mapValue string) string {
		switch {
		case arrayElem != "":
			return "[]" + arrayElem
		case schemaName != "":
			return schemaName
		case mapValue != "":
			return "map[string]" + mapValue
		}
		return ""
	}
	var out []string
	for _, v := range r.Variants {
		if n := name(v.SchemaName, v.ArrayElem, v.MapValueType); n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		if n := name(r.SchemaName, r.ArrayElem, r.MapValueType); n != "" {
			out = append(out, n)
		}
	}
	return out
}
