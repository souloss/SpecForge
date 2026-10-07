// Package infer 的画像自动学习：抽样深读若干接口，产出仓库约定画像候选。
package infer

import (
	"context"
	"encoding/json"
	"fmt"
)

// LearnProfile 对抽样接口深读，产出仓库约定画像（设计文档 §3.3 约定画像 Agent）。
//
// 输入 sampleOps 是「method path + handler 源码」的抽样集；输出结构化的画像候选。
// LLM 只负责从证据里**归纳**约定（响应汇聚点符号、鉴权中间件、信封结构），
// 候选符号由 engine 在代码图上复核（不存在的符号丢弃）。离线（p == nil）返回 ErrNoProvider。
func LearnProfile(ctx context.Context, p Provider, sampleOps []SampleOp) (*ProfileCandidate, error) {
	if p == nil {
		return nil, ErrNoProvider
	}
	system := prompt("learn-profile.system.md") + prompt("learn-profile.schema.json")

	ev, err := json.Marshal(sampleOps)
	if err != nil {
		return nil, fmt.Errorf("infer: marshal sample ops: %w", err)
	}
	resp, err := p.Complete(ctx, Request{System: system + "\n", Prompt: string(ev)})
	if err != nil {
		return nil, err
	}
	var out ProfileCandidate
	if err := json.Unmarshal(extractJSON(resp.Text), &out); err != nil {
		return nil, fmt.Errorf("infer: profile output invalid: %w", err)
	}
	return &out, nil
}

// SampleOp 画像学习的抽样接口（method + path + 证据摘要）。
type SampleOp struct {
	Method   string          `json:"method"`   // HTTP 方法
	Path     string          `json:"path"`     // OpenAPI 路径
	Evidence string          `json:"evidence"` // 证据摘要（handler 注释等）
	Sources  []SourceSnippet `json:"sources"`  // handler 及其直接被调函数源码
}

// ProfileCandidate 画像候选（与 profile.Profile 结构对齐的可序列化子集）。
type ProfileCandidate struct {
	Framework      string              `json:"framework"`
	ResponseSinks  []SinkCandidate     `json:"responseSinks"`
	AuthMiddleware map[string]AuthCand `json:"authMiddleware"`
	Envelope       *EnvelopeCandidate  `json:"envelope,omitempty"`
}

// SinkCandidate 响应汇聚点候选。
type SinkCandidate struct {
	Symbol    string `json:"symbol"`
	Signature string `json:"signature"`
	DataSlot  int    `json:"dataSlot"`
	ErrSlot   int    `json:"errSlot"`
	Status    int    `json:"status"`
}

// AuthCand 鉴权中间件候选。
type AuthCand struct {
	Header   string `json:"header"`
	Scheme   string `json:"scheme"`
	Required bool   `json:"required"`
}

// EnvelopeCandidate 信封候选。
type EnvelopeCandidate struct {
	Type        string            `json:"type"`
	Properties  map[string]string `json:"properties"`
	DataSlot    string            `json:"dataSlot"`
	SuccessCode int               `json:"successCode"`
}
