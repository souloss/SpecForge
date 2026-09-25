// Package infer 的画像自动学习：抽样深读若干接口，产出仓库约定画像候选。
package infer

import (
	"encoding/json"
	"fmt"
)

// LearnProfile 对抽样接口深读，产出仓库约定画像（设计文档 §3.3 约定画像 Agent）。
//
// 输入 sampleOps 是「method path + 证据摘要」的抽样集（engine 侧由 gap 或
// 路由抽样给出）；输出结构化的画像候选。LLM 只负责从证据里**归纳**约定
// （响应汇聚点符号、鉴权中间件、信封结构），不发明任何不存在的符号。
// 离线（p == nil）返回 ErrNoProvider。
func LearnProfile(p Provider, sampleOps []SampleOp) (*ProfileCandidate, error) {
	if p == nil {
		return nil, ErrNoProvider
	}
	system := "You are analyzing a Go+Fiber repository to learn its API conventions. " +
		"Given sample operations and code evidence, identify the RESPONSE SINK symbols " +
		"(functions that write the HTTP response, e.g. code.WriteResponse), the AUTH " +
		"MIDDLEWARE (functions wrapping routes for auth), and the response ENVELOPE " +
		"(the wrapper object around business data). Only report symbols that appear " +
		"in the evidence. Respond as JSON matching the output schema."

	ev, err := json.Marshal(sampleOps)
	if err != nil {
		return nil, fmt.Errorf("infer: marshal sample ops: %w", err)
	}
	schema := []byte(learnProfileSchema)
	raw, err := p.Complete(system, string(ev), schema)
	if err != nil {
		return nil, err
	}
	var out ProfileCandidate
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("infer: profile output invalid: %w", err)
	}
	return &out, nil
}

// SampleOp 画像学习的抽样接口（method + path + 证据摘要）。
type SampleOp struct {
	Method   string `json:"method"`
	Path     string `json:"path"`
	Evidence string `json:"evidence"`
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

// learnProfileSchema 画像候选的输出 schema。
const learnProfileSchema = `{
  "type": "object",
  "properties": {
    "framework": {"type": "string"},
    "responseSinks": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "symbol": {"type": "string"},
          "signature": {"type": "string"},
          "dataSlot": {"type": "integer"},
          "errSlot": {"type": "integer"},
          "status": {"type": "integer"}
        },
        "required": ["symbol"]
      }
    },
    "authMiddleware": {
      "type": "object",
      "additionalProperties": {
        "type": "object",
        "properties": {
          "header": {"type": "string"},
          "scheme": {"type": "string"},
          "required": {"type": "boolean"}
        }
      }
    },
    "envelope": {
      "type": "object",
      "properties": {
        "type": {"type": "string"},
        "properties": {"type": "object"},
        "dataSlot": {"type": "string"},
        "successCode": {"type": "integer"}
      }
    }
  }
}`
