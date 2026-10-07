// Package infer 推断层: LLM provider 抽象、调用缓存与档位 2/3 的任务协议
// （设计文档 §6 Agent 编排协议）。
//
// 分层：Provider（模型调用）→ CachedProvider（内容寻址缓存 + 工具读集校验）→
// ResolveGaps / LearnProfile / EnrichOperation（任务协议与结构校验）。
// 无 API 配置时全部调用走 ErrNoProvider → 上层显式降级，
// 绝不静默编造（设计文档 §6.5 静默降质被协议禁止）。
package infer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// ErrNoProvider 无可用 LLM 配置（离线模式）。
var ErrNoProvider = errors.New("infer: no LLM provider configured (offline mode)")

// ErrToolsUnsupported provider 不支持工具调用（档位3 不可用，上层退回档位2）。
var ErrToolsUnsupported = errors.New("infer: provider does not support tools")

// ErrToolBudgetExhausted 档位3 工具循环用尽最大轮数或单次调用时限仍未作答（上层退回档位2，凭已注入的证据作答）。
var ErrToolBudgetExhausted = errors.New("infer: tool loop exhausted its turn or time budget")

// Request 一次结构化补全请求。Tools 非空即档位3 多轮工具循环（MaxTurns 限定轮数）。
type Request struct {
	System   string     // system prompt（稳定前缀：放任务规则与大块静态上下文，利于网关前缀缓存）
	Prompt   string     // 用户消息（本次任务的可变部分）
	Schema   []byte     // 期望输出的 JSON Schema；nil = 不下发（schema 已内嵌 prompt 时）
	Tools    []ToolSpec // 档位3 工具集；nil = 单次调用
	MaxTurns int        // 工具循环最大轮数（Tools 非空时有效）
}

// Response 一次补全的结果。
type Response struct {
	Text  []byte // 模型最终输出文本（期望为 JSON，结构校验由任务协议层承担）
	Usage Usage  // 本次调用的 token 用量（缓存命中时为原调用的用量，计入「节省」）
}

// Provider LLM 供应方抽象。实现须并发安全。
type Provider interface {
	// Complete 执行一次补全；ctx 取消时应尽快返回 ctx.Err()。
	// 不支持工具的实现遇到 len(req.Tools) > 0 返回 ErrToolsUnsupported。
	Complete(ctx context.Context, req Request) (Response, error)
	// Name 供应方与模型标识（计入缓存键与 memo 指纹）。
	Name() string
}

// UsageReporter 可上报累计用量的 Provider（可选接口，engine 以类型断言判定）。
type UsageReporter interface {
	// LLMUsage 返回自创建以来的累计用量。
	LLMUsage() Usage
}

// Usage LLM 用量（观测字段，非协议数据）。
type Usage struct {
	Calls        int   `json:"calls"`         // 实际发生的模型调用次数
	InputTokens  int64 `json:"input_tokens"`  // 累计输入 token
	OutputTokens int64 `json:"output_tokens"` // 累计输出 token
}

// TotalTokens 输入+输出 token 总和。
func (u Usage) TotalTokens() int64 { return u.InputTokens + u.OutputTokens }

// Add 累加另一份用量。
func (u *Usage) Add(o Usage) {
	u.Calls += o.Calls
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
}

// LLMUsage 旧名兼容别名（engine/CLI 既有引用）。
type LLMUsage = Usage

// Configured 是否配置了 LLM 凭据（不构造 provider、不初始化框架，供 doctor 等快速检查）。
func Configured() bool {
	return os.Getenv(envAuthToken) != "" || os.Getenv(envAPIKey) != ""
}

// ModelName 将使用的模型 ID（ANTHROPIC_MODEL 或默认值）。
func ModelName() string { return envOr(envModel, defaultModel) }

// NewProviderFromEnv 从环境变量构造 Provider（anthropic 协议网关）；未配置返回 nil（离线模式）。
func NewProviderFromEnv() Provider {
	return NewGenkitProvider()
}

// enrichSummaryMaxLen / enrichDescriptionMaxLen 语义增强输出长度上限（schema 约束，防长文本灌水）。
const (
	enrichSummaryMaxLen     = 120
	enrichDescriptionMaxLen = 600
)

// Enrichment 语义增强产出（F8：只含描述，不含任何结构性事实）。
type Enrichment struct {
	Summary     string `json:"summary"`     // 一行摘要
	Description string `json:"description"` // 可选的补充描述
}

// EnrichOperation 对单个 operation 做语义增强；evidence 为 handler 源码切片。
// 离线返回 ErrNoProvider——上层降级为 godoc 静态增强（零成本路径）。
func EnrichOperation(ctx context.Context, p Provider, op, evidence string) (*Enrichment, error) {
	if p == nil {
		return nil, ErrNoProvider
	}
	resp, err := p.Complete(ctx, Request{
		System: prompt("enrich.system.md"),
		Prompt: fmt.Sprintf("operation: %s\nsource:\n%s\n", op, evidence),
	})
	if err != nil {
		return nil, err
	}
	var out Enrichment
	if err := json.Unmarshal(extractJSON(resp.Text), &out); err != nil {
		return nil, fmt.Errorf("infer: enrichment output invalid: %w", err)
	}
	// 防线 2: 结构校验（§6.3）
	if out.Summary == "" {
		return nil, errors.New("infer: enrichment missing summary")
	}
	out.Summary = truncateRunes(out.Summary, enrichSummaryMaxLen)
	out.Description = truncateRunes(out.Description, enrichDescriptionMaxLen)
	return &out, nil
}

// truncateRunes 按字符（非字节）截断，避免切断多字节 UTF-8。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
