// Package infer 推断层: LLM provider 接口与档位 2/3 的任务卡协议
// （设计文档 §6 Agent 编排协议）。
//
// P0/P1 实现: Provider 接口 + OpenAI 兼容 HTTP 实现 + 离线降级。
// 静态管线（档位 1）已覆盖本仓库全部 operation；LLM 层仅在
// 静态缺口（any 定型、语义增强、路由消歧）需要时介入。
// 无 API 配置时全部调用走 ErrNoProvider → 上层显式降级，
// 绝不静默编造（设计文档 §6.5 静默降质被协议禁止）。
package infer

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNoProvider 无可用 LLM 配置（离线模式）。
var ErrNoProvider = errors.New("infer: no LLM provider configured (offline mode)")

// Provider LLM 供应方抽象（结构化补全）。
type Provider interface {
	// Complete 结构化补全。system 与 prompt 注入; schema 为 JSON Schema
	// （输出约束；具体是否被模型侧强制取决于插件，事后结构校验由上层承担）。
	Complete(system, prompt string, schema []byte) ([]byte, error)
	Name() string
}

// UsageReporter 可上报 token 用量的 Provider（可选接口）。
//
// engine 以类型断言判定：非 UsageReporter 的 Provider（如离线 mock）用量记 0。
// 用量是 provider 内部跨调用累加的，供 CLI 打印 token 消耗与成本估算。
type UsageReporter interface {
	// LLMUsage 返回该 provider 从创建至今的累计调用次数与 token 用量。
	LLMUsage() LLMUsage
}

// LLMUsage 一次运行的 LLM 累计用量（观测字段，非协议数据）。
type LLMUsage struct {
	Calls        int   // 实际发生的 LLM 调用次数
	InputTokens  int64 // 累计输入 token
	OutputTokens int64 // 累计输出 token
}

// TotalTokens 输入+输出 token 总和。
func (u LLMUsage) TotalTokens() int64 { return u.InputTokens + u.OutputTokens }

// NewProviderFromEnv 从环境变量构造 Provider。
// 优先走 Genkit Provider（Z.ai GLM）；未配置返回 nil（离线模式）。
func NewProviderFromEnv() Provider {
	return NewGenkitProvider()
}

// TaskCard 档位 2/3 的任务卡（设计文档 §10.4）。
type TaskCard struct {
	Task       string   `json:"task"`
	Operation  string   `json:"op"`
	Tier       string   `json:"tier"`
	Budget     Budget   `json:"budget"`
	ProfileRef string   `json:"profile_ref,omitempty"`
	SliceRef   string   `json:"slice_ref,omitempty"`
	Unresolved []string `json:"unresolved,omitempty"`
}

// Budget 硬预算（步数/token，超限降级，设计文档 §6.5）。
type Budget struct {
	Steps  int `json:"steps"`
	Tokens int `json:"tokens"`
}

// EnrichTask 语义增强任务卡（档位 2，批量 20 op/次）。
func EnrichTask(op, summaryHint string) TaskCard {
	return TaskCard{
		Task: "enrich_operation", Operation: op, Tier: "2",
		Budget: Budget{Steps: 1, Tokens: 5000},
	}
}

// EnrichOperation 对单个 operation 做语义增强。
// 离线（无 provider）时返回 ErrNoProvider——上层降级为 godoc 静态增强
// （engine 已实现，零成本路径）。
func EnrichOperation(p Provider, op, evidenceSummary string) (map[string]interface{}, error) {
	if p == nil {
		return nil, ErrNoProvider
	}
	system := "You are an API documentation writer. Given code evidence, " +
		"produce a concise operation summary. Never invent structural facts " +
		"(fields, types, status codes) — only descriptions."
	prompt := fmt.Sprintf("operation: %s\nevidence: %s\n", op, evidenceSummary)
	schema := []byte(`{"type":"object","properties":{` +
		`"summary":{"type":"string","maxLength":120},` +
		`"description":{"type":"string","maxLength":600}},` +
		`"required":["summary"],"additionalProperties":false}`)
	raw, err := p.Complete(system, prompt, schema)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("infer: enrichment output invalid: %w", err)
	}
	// 防线 2: 结构校验（§6.3）
	if _, ok := out["summary"].(string); !ok {
		return nil, errors.New("infer: enrichment missing summary")
	}
	return out, nil
}
