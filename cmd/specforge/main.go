// Package main 是 SpecForge CLI 入口（cobra）。
//
// 两个接口、一套命令：人类模式（stdout 可读摘要，进度日志走 stderr）与机器模式
// （--json：stdout 恰好输出一个 JSON 信封 {schema_version, command, ok, data, error}）。
// 退出码与 error_code 是机器契约，见 exitCodeHelp 与 errorCodeHelp。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/specforge/specforge/internal/engine"
)

// 退出码（机器契约，发布后不可改语义；设计文档 §3.2 接入层 + 通用 0/1/2 约定）。
const (
	exitOK             = 0  // 成功
	exitInternal       = 1  // 内部/未知错误（含确定性自检失败）
	exitUsage          = 2  // 命令行参数无效
	exitDegraded       = 10 // 部分降级：LLM 调用失败或 operation 分析失败被隔离（产物已写出）
	exitBudgetExceeded = 20 // LLM 预算耗尽，仍有缺口未处理（产物已写出）
	exitConfig         = 30 // 配置/输入错误：无 go.mod、画像不合法、--llm 但未配置凭据、找不到产物
	exitGateFailed     = 40 // 质量闸门未通过：eval 指标低于 --fail-under
)

// error_code 取值（机器契约：--json 信封 error.code 字段）。
const (
	codeInvalidArgs    = "invalid_args"         // 命令行参数错误
	codeConfigInvalid  = "config_invalid"       // 仓库/画像/配置不合法
	codeLLMNotConfig   = "llm_not_configured"   // 请求了 LLM 但未配置凭据
	codeNotFound       = "not_found"            // 找不到产物或 operation
	codeInternal       = "internal_error"       // 内部错误
	codeDeterminism    = "determinism_failed"   // 确定性自检失败
	codeGateFailed     = "quality_gate_failed"  // eval 质量闸门未通过
	codeLLMDegraded    = "llm_degraded"         // LLM 调用或 operation 分析失败（--fail-on-degraded）
	codeBudgetExceeded = "llm_budget_exhausted" // 预算耗尽仍有缺口（--fail-on-degraded）
)

// exitCodeHelp 根命令帮助中的退出码说明（与上方常量一一对应）。
const exitCodeHelp = `Exit codes:
  0   success
  1   internal error (including determinism self-check failure)
  2   invalid command-line usage
  10  degraded: LLM calls or operation analyses failed; outputs still written (gen --fail-on-degraded)
  20  LLM budget exhausted with gaps left; outputs still written (gen --fail-on-degraded)
  30  configuration/input error: no go.mod, invalid profile, --llm without credentials, missing gen output
  40  quality gate failed (eval --fail-under)
  130 operation canceled by the user or caller`

// jsonContractHelp 根命令帮助中的机器模式说明。
const jsonContractHelp = `Machine mode (--json):
  stdout carries exactly one JSON object; logs never go to stdout.
    {"schema_version":"1","command":"gen","ok":true,"data":{...}}
    {"schema_version":"1","command":"gen","ok":false,"error":{"code":"config_invalid","message":"...","hint":"...","exit_code":30}}
  gen route counts distinguish registrations from operation identities:
    routes_resolved = registrations ready for static contract analysis;
    routes_unique = known method/path identities, including routes whose handlers are unresolved;
    routes_collapsed = extra registrations sharing a method/path identity;
    unresolved_routes = source-located route diagnostics (an empty array when none remain).
  error.code values: invalid_args, config_invalid, llm_not_configured, not_found, internal_error,
  determinism_failed, quality_gate_failed, llm_degraded, llm_budget_exhausted, canceled.`

// cliError 带退出码与 error_code 的命令错误（人类通道 Message + Hint，机器通道 Code + Exit）。
type cliError struct {
	Exit    int    // 进程退出码
	Code    string // 机器可读 error_code
	Message string // 发生了什么
	Hint    string // 用户该怎么做（可空）
}

// Error 实现 error。
func (e *cliError) Error() string { return e.Message }

// newErr 构造 cliError。
func newErr(exit int, code, msg, hint string) *cliError {
	return &cliError{Exit: exit, Code: code, Message: msg, Hint: hint}
}

// envelope --json 模式的唯一输出对象。
type envelope struct {
	SchemaVersion string       `json:"schema_version"`  // 契约版本
	Command       string       `json:"command"`         // 执行的命令（如 "gen"、"cache clean"）
	OK            bool         `json:"ok"`              // 是否成功（退出码为 0）
	Data          any          `json:"data,omitempty"`  // 命令结果（失败时也可能有，如 eval 闸门失败时的指标）
	Error         *envelopeErr `json:"error,omitempty"` // 失败详情
}

// envelopeErr 信封中的错误详情。
type envelopeErr struct {
	Code     string `json:"code"`           // error_code
	Message  string `json:"message"`        // 人类可读原因
	Hint     string `json:"hint,omitempty"` // 修复建议
	ExitCode int    `json:"exit_code"`      // 进程退出码
}

// app CLI 运行上下文：输出通道与机器模式的待输出数据。
type app struct {
	json   bool      // 机器模式
	stdout io.Writer // 数据通道
	stderr io.Writer // 消息通道（日志、错误、提示）
	data   any       // 机器模式下命令产出的数据（由 run 统一装进信封输出）
}

// emit 输出命令结果：机器模式暂存到信封，人类模式调用 human 立即打印到 stdout。
func (a *app) emit(data any, human func(w io.Writer)) {
	if a.json {
		a.data = data
		return
	}
	human(a.stdout)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run 执行 CLI 并返回退出码（可测试入口：不直接 os.Exit）。
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := &app{stdout: stdout, stderr: stderr}
	root := newRootCmd(a)
	root.SetArgs(args)
	root.SetContext(ctx)
	cmd, err := root.ExecuteC()
	if cmd == nil {
		cmd = root
	}
	var ce *cliError
	if errors.Is(err, context.Canceled) {
		ce = newErr(exitCanceled, codeCanceled, "operation canceled", "")
	} else if err != nil && !errors.As(err, &ce) {
		ce = newErr(exitUsage, codeInvalidArgs, err.Error(), "run '"+cmd.CommandPath()+" --help' for usage")
	}
	if a.json {
		env := envelope{SchemaVersion: engine.SchemaVersion, Command: commandName(cmd), OK: ce == nil, Data: a.data}
		if ce != nil {
			env.Error = &envelopeErr{Code: ce.Code, Message: ce.Message, Hint: ce.Hint, ExitCode: ce.Exit}
		}
		writeJSON(stdout, env)
	} else if ce != nil {
		fmt.Fprintf(stderr, "error: %s\n", ce.Message)
		if ce.Hint != "" {
			fmt.Fprintf(stderr, "hint:  %s\n", ce.Hint)
		}
	}
	if ce != nil {
		return ce.Exit
	}
	return exitOK
}

// commandName 命令路径去掉根名（"specforge cache clean" → "cache clean"）。
func commandName(cmd *cobra.Command) string {
	return strings.TrimPrefix(strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()), " ")
}

// Version CLI 版本（与引擎版本一致）。
const Version = engine.Version

// newRootCmd 构造命令树。
func newRootCmd(a *app) *cobra.Command {
	root := &cobra.Command{
		Use:   "specforge",
		Short: "Agent-native OpenAPI generator for source repositories (zero annotations)",
		Long: `SpecForge extracts the real HTTP contract of a Go (Fiber, Gin or chi) service straight from source code —
routes, parameters, request bodies, response envelopes and business error codes — and compiles it
into a deterministic OpenAPI 3.1 document in which every fact is traceable to a file:line.
No annotations are required. Non-Go source trees can use the generic LLM frontend with --llm.

Typical workflow:
  1. specforge doctor                 check toolchain, repository and LLM configuration
  2. specforge gen                    analyze the repository, write openapi.yaml / report.md / operations.json
  3. specforge ops --low-confidence   find operations that need attention
  4. specforge explain METHOD PATH    see the evidence behind one operation
  5. specforge eval ...               score against a hand-written spec (CI quality gate)

` + exitCodeHelp + "\n\n" + jsonContractHelp,
		Example: `  specforge gen --repo ./my-service
  specforge gen --repo . --llm
  specforge ops --low-confidence
  specforge explain POST /ipo/v1/OrderCheck
  specforge eval --truth truth.yaml --spec .specforge/out/openapi.yaml --fail-under 0.9`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.PersistentFlags().BoolVar(&a.json, "json", false, "machine mode: print exactly one JSON envelope on stdout (see 'specforge --help')")
	root.AddCommand(
		newGenCmd(a),
		newOpsCmd(a),
		newExplainCmd(a),
		newEvalCmd(a),
		newVerifyCmd(a),
		newCacheCmd(a),
		newDoctorCmd(a),
		newVersionCmd(a),
	)
	installHelp(root)
	return root
}

// writeJSON 缩进 JSON（单对象，结尾换行，不转义 HTML 字符）。
func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// isTerminal 文件是否连着终端（字符设备）。
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
