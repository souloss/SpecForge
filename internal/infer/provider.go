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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// ErrNoProvider 无可用 LLM 配置（离线模式）。
var ErrNoProvider = errors.New("infer: no LLM provider configured (offline mode)")

// Provider LLM 供应方抽象（temperature=0 的结构化补全）。
type Provider interface {
	// Complete 结构化补全。system 与 prompt 注入; schema 为 JSON Schema
	// 约束解码（防线 1，设计文档 §6.3）。
	Complete(system, prompt string, schema []byte) ([]byte, error)
	Name() string
}

// HTTPProvider OpenAI 兼容 /chat/completions 实现。
type HTTPProvider struct {
	BaseURL string // 如 https://api.example.com/v1
	APIKey  string
	Model   string
	Client  *http.Client
}

// NewProviderFromEnv 从环境变量构造（SPECFORGE_LLM_BASE_URL 等）。
// 未配置时返回 nil（离线模式）。
func NewProviderFromEnv() Provider {
	base := os.Getenv("SPECFORGE_LLM_BASE_URL")
	key := os.Getenv("SPECFORGE_LLM_API_KEY")
	if base == "" || key == "" {
		return nil
	}
	return &HTTPProvider{
		BaseURL: base,
		APIKey:  key,
		Model:   envOr("SPECFORGE_LLM_MODEL", "glm-4.6"),
		Client:  &http.Client{Timeout: 120 * time.Second},
	}
}

// Name 实现 Provider。
func (p *HTTPProvider) Name() string { return "http:" + p.Model }

// Complete 实现 Provider: JSON Schema 约束 + temperature 0。
func (p *HTTPProvider) Complete(system, prompt string, schema []byte) ([]byte, error) {
	msgs := []map[string]interface{}{
		{"role": "system", "content": system},
		{"role": "user", "content": prompt},
	}
	body := map[string]interface{}{
		"model":       p.Model,
		"messages":    msgs,
		"temperature": 0,
	}
	if len(schema) > 0 {
		body["response_format"] = map[string]interface{}{
			"type": "json_schema",
			"json_schema": map[string]interface{}{
				"name":   "specforge_fact",
				"schema": json.RawMessage(schema),
			},
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", p.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("infer: llm call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("infer: llm http %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, errors.New("infer: empty choices")
	}
	return []byte(out.Choices[0].Message.Content), nil
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

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
