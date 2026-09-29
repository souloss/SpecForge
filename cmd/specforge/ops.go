package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/specforge/specforge/internal/compiler"
	"github.com/specforge/specforge/internal/engine"
)

// lowConfidenceLine ops --low-confidence 的阈值（与 report.md 的低置信口径一致）。
const lowConfidenceLine = 0.8

// maxSuggestions explain 找不到 operation 时最多提示的相近路径数。
const maxSuggestions = 5

// outputFlags ops / explain 共用的「定位 gen 产物」选项。
type outputFlags struct {
	repo string // 仓库根（推导默认产物目录）
	out  string // 产物目录（显式指定时优先）
}

// register 注册 --repo / --out。
func (o *outputFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&o.repo, "repo", ".", "repository root (locates the default output directory)")
	cmd.Flags().StringVarP(&o.out, "out", "o", "", "gen output directory (default: <repo>/.specforge/out)")
}

// load 读取 operations.json；缺失/损坏/版本不符时返回带修复建议的错误。
func (o *outputFlags) load() ([]compiler.Operation, error) {
	dir := o.out
	if dir == "" {
		dir = defaultOutDir(o.repo)
	}
	f, err := engine.ReadOperations(dir)
	if err != nil {
		return nil, newErr(exitConfig, codeNotFound, "cannot read generated operations in "+dir+": "+err.Error(),
			"run 'specforge gen' first (or pass --out pointing at its output directory)")
	}
	return f.Operations, nil
}

// opRow ops 命令的一行（机器模式输出元素）。
type opRow struct {
	Method     string   `json:"method"`      // HTTP 方法
	Path       string   `json:"path"`        // 路径模板
	Confidence float64  `json:"confidence"`  // 置信度
	Summary    string   `json:"summary"`     // 摘要
	Unknowns   []string `json:"unknowns"`    // 未解析项
	Body       string   `json:"body"`        // 请求体 schema（无则空）
	Response   string   `json:"response"`    // 成功响应体 schema（无则空）
	ErrorCodes []int    `json:"error_codes"` // 已确定的业务错误码
}

// newOpsCmd ops：列出 gen 产出的全部 operation（可按置信度/缺口过滤）。
func newOpsCmd(a *app) *cobra.Command {
	var of outputFlags
	var lowOnly, gapsOnly bool
	cmd := &cobra.Command{
		Use:   "ops",
		Short: "List generated operations with confidence and open gaps",
		Long: `List every operation from the last 'gen' run: method, path, confidence, request/response schema,
known business error codes and unresolved gaps. Reads operations.json; does not re-analyze.

Confidence is 1.0 when everything was resolved statically. It drops when a response payload is
'any', an error variable cannot be traced to constants, or no response writer was reached.
Operations below 0.8 are the ones listed in report.md.`,
		Example: `  specforge ops
  specforge ops --low-confidence
  specforge ops --gaps --json | jq -r '.data[] | "\(.method) \(.path): \(.unknowns | join("; "))"'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ops, err := of.load()
			if err != nil {
				return err
			}
			rows := make([]opRow, 0, len(ops))
			for i := range ops {
				op := &ops[i]
				if (lowOnly && op.Confidence >= lowConfidenceLine) || (gapsOnly && len(op.Unknowns) == 0) {
					continue
				}
				rows = append(rows, rowOf(op))
			}
			a.emit(rows, func(w io.Writer) { printOps(w, rows, len(ops)) })
			return nil
		},
	}
	of.register(cmd)
	cmd.Flags().BoolVar(&lowOnly, "low-confidence", false, "only operations with confidence below 0.8")
	cmd.Flags().BoolVar(&gapsOnly, "gaps", false, "only operations with unresolved gaps")
	return cmd
}

// rowOf operation → ops 行。
func rowOf(op *compiler.Operation) opRow {
	r := opRow{Method: op.Method, Path: op.Path, Confidence: op.Confidence, Summary: op.Summary,
		Unknowns: op.Unknowns, Body: bodyLabel(op.Body), ErrorCodes: []int{}}
	for _, resp := range op.Responses {
		if s := responseLabel(resp); s != "" && r.Response == "" {
			r.Response = s
		}
		for _, c := range resp.Codes {
			if c != 0 {
				r.ErrorCodes = append(r.ErrorCodes, c)
			}
		}
	}
	return r
}

// bodyLabel 请求体的简短描述（信封时显示「信封{字段: 业务体}」）。
func bodyLabel(b *compiler.BodyOut) string {
	if b == nil {
		return ""
	}
	if b.EnvelopeName != "" {
		return b.EnvelopeName + "{" + b.DataField + ": " + b.SchemaName + "}"
	}
	return b.SchemaName
}

// responseLabel 响应体的简短描述。
func responseLabel(r compiler.ResponseOut) string {
	switch {
	case r.ArrayElem != "":
		return "[]" + r.ArrayElem
	case r.SchemaName != "":
		return r.SchemaName
	case r.MapValueType != "":
		return "map[string]" + r.MapValueType
	}
	return ""
}

// printOps 人类模式：每行一个 operation（制表对齐，可 grep/awk）。
func printOps(w io.Writer, rows []opRow, total int) {
	for _, r := range rows {
		gaps := ""
		if len(r.Unknowns) > 0 {
			gaps = fmt.Sprintf("  gaps=%d", len(r.Unknowns))
		}
		fmt.Fprintf(w, "%-6s %-50s %.2f  body=%s  resp=%s%s\n", r.Method, r.Path, r.Confidence, orDash(r.Body), orDash(r.Response), gaps)
	}
	fmt.Fprintf(w, "%d of %d operations\n", len(rows), total)
}

// orDash 空串显示为 "-"。
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// newExplainCmd explain：展示单个 operation 的契约与证据链。
func newExplainCmd(a *app) *cobra.Command {
	var of outputFlags
	cmd := &cobra.Command{
		Use:   "explain METHOD PATH",
		Short: "Show the contract and evidence behind one generated operation",
		Long: `Show everything SpecForge concluded about one operation and why: parameters with their source
(e.g. fiber:Query, route-template, middleware), request body, every response row with the file:line
where it is written, evidence lines (route registration, LLM-verified facts) and unresolved gaps.

METHOD is case-insensitive. PATH must match the OpenAPI path exactly; quote paths that contain
template parameters, e.g. '/ipo/v{version}/IPOList'. Use 'specforge ops' to list available paths.
Reads operations.json from the last 'gen' run.`,
		Example: `  specforge explain POST /ipo/v1/OrderCheck
  specforge explain get '/ipoAdmin/v{version}/IPOList' --out docs/api
  specforge explain POST /ipo/v1/OrderCheck --json | jq '.data.responses'`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ops, err := of.load()
			if err != nil {
				return err
			}
			method := strings.ToUpper(args[0])
			for i := range ops {
				if ops[i].Method == method && ops[i].Path == args[1] {
					op := &ops[i]
					a.emit(op, func(w io.Writer) { printOperation(w, op) })
					return nil
				}
			}
			return newErr(exitConfig, codeNotFound, fmt.Sprintf("operation %s %s not found", method, args[1]), suggestOps(ops, args[1]))
		},
	}
	of.register(cmd)
	return cmd
}

// suggestOps 找不到时给出路径末段相同的候选。
func suggestOps(ops []compiler.Operation, path string) string {
	tail := path[strings.LastIndex(path, "/")+1:]
	var c []string
	for _, op := range ops {
		if tail != "" && strings.HasSuffix(op.Path, "/"+tail) {
			c = append(c, op.Method+" "+op.Path)
		}
	}
	if len(c) == 0 {
		return "list available operations with 'specforge ops'"
	}
	if len(c) > maxSuggestions {
		c = c[:maxSuggestions]
	}
	return "did you mean: " + strings.Join(c, " | ")
}

// printOperation 人类模式的 operation 证据视图。
func printOperation(w io.Writer, op *compiler.Operation) {
	fmt.Fprintf(w, "%s %s  (operationId %s, confidence %.2f)\n", op.Method, op.Path, op.OperationID, op.Confidence)
	if op.Summary != "" {
		fmt.Fprintf(w, "  summary: %s\n", op.Summary)
	}
	for _, e := range op.Evidence {
		fmt.Fprintf(w, "  evidence: %s\n", e)
	}
	for _, p := range op.Params {
		fmt.Fprintf(w, "  param %-6s %-20s %-8s required=%v  ← %s\n", p.In, p.Name, p.Type, p.Required, p.Origin)
	}
	if op.Body != nil {
		fmt.Fprintf(w, "  request body: %s\n", bodyLabel(op.Body))
	}
	for _, r := range op.Responses {
		fmt.Fprintf(w, "  response %s: %s\n", r.Status, r.Description)
		for _, s := range r.Sinks {
			fmt.Fprintf(w, "    written at %s\n", s)
		}
	}
	for _, u := range op.Unknowns {
		fmt.Fprintf(w, "  unresolved: %s\n", u)
	}
}
