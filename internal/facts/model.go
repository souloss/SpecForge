// Package facts 实现 API Fact Graph 数据模型（设计文档 §4）。
package facts

import "fmt"

// FactKind 事实类型枚举（封闭集合，设计文档 §4.1）。
type FactKind string

const (
	KindService      FactKind = "service"
	KindProfile      FactKind = "profile"
	KindRoute        FactKind = "route"
	KindContract     FactKind = "contract"
	KindSchema       FactKind = "schema"
	KindSecurity     FactKind = "security"
	KindErrorCatalog FactKind = "errorcatalog"
	KindEnrichment   FactKind = "enrichment"
)

// FactSource 事实来源。
type FactSource string

const (
	SourceStatic  FactSource = "static"
	SourceLLM     FactSource = "llm"
	SourceRuntime FactSource = "runtime"
	SourceHuman   FactSource = "human"
)

// Evidence 证据三元组（file + 行范围 + 内容哈希，设计文档 §4.1）。
type Evidence struct {
	File      string `yaml:"file"`
	StartLine int    `yaml:"startLine"`
	EndLine   int    `yaml:"endLine"`
	BlobSHA   string `yaml:"blobSha"`
	Quote     string `yaml:"quote,omitempty"`
}

// Fact 一条事实。
type Fact struct {
	ID         string      `yaml:"id"`
	Kind       FactKind    `yaml:"kind"`
	Value      interface{} `yaml:"value"`
	Source     FactSource  `yaml:"source"`
	Confidence float64     `yaml:"confidence"`
	Evidence   []Evidence  `yaml:"evidence"`
	Status     string      `yaml:"status"`
}

// ContractPayload contract 事实载荷（设计文档 §4.1 ContractPayload）。
type ContractPayload struct {
	OperationID string         `yaml:"operationId"`
	Tags        []string       `yaml:"tags,omitempty"`
	Params      []ParamFact    `yaml:"parameters,omitempty"`
	RequestBody *BodyFact      `yaml:"requestBody,omitempty"`
	Responses   []ResponseFact `yaml:"responses"`
	Security    []string       `yaml:"security,omitempty"`
	// Gaps 静态分析未能定型、需 LLM 兜底采集的缺口清单（档位判定输入）。
	// 空 = 全静态可定型（档位1，0 token）；非空 = 候选兜底对象。
	Gaps []string `yaml:"gaps,omitempty"`
}

// ParamFact 参数事实。
type ParamFact struct {
	In       string `yaml:"in"`
	Name     string `yaml:"name"`
	Required bool   `yaml:"required"`
	Type     string `yaml:"type"`
	Format   string `yaml:"format,omitempty"`
	Origin   string `yaml:"origin,omitempty"`
}

// BodyFact 请求体事实。
type BodyFact struct {
	ContentType string `yaml:"contentType"`
	SchemaType  string `yaml:"schemaType"`
}

// ResponseFact 响应矩阵行（status × envelope × schema，设计文档 §4.2）。
type ResponseFact struct {
	Status     int       `yaml:"status"`
	Envelope   *Envelope `yaml:"envelope,omitempty"`
	SchemaType string    `yaml:"schemaType,omitempty"`
	HasBody    bool      `yaml:"hasBody"`
	Sink       string    `yaml:"sink"`
}

// Envelope 业务错误码信封。
type Envelope struct {
	Code    int    `yaml:"code"`
	CodeRef string `yaml:"codeRef,omitempty"`
	Msg     string `yaml:"msg,omitempty"`
}

// RoutePayload route 事实载荷。
type RoutePayload struct {
	Method     string   `yaml:"method"`
	Path       string   `yaml:"path"`
	Handler    string   `yaml:"handler"`
	Middleware []string `yaml:"middleware,omitempty"`
}

// SecurityPayload security 事实载荷。
type SecurityPayload struct {
	MiddlewareSymbol string `yaml:"middleware"`
	SchemeType       string `yaml:"schemeType"`
	Header           string `yaml:"header,omitempty"`
	ScopeArg         string `yaml:"scopeArg,omitempty"`
	Required         bool   `yaml:"required"`
}

// ErrorCatalogPayload 错误码目录载荷。
type ErrorCatalogPayload struct {
	Package string      `yaml:"package"`
	Codes   []ErrorCode `yaml:"codes"`
}

// ErrorCode 单个错误码。
type ErrorCode struct {
	Symbol string `yaml:"symbol"`
	Code   int    `yaml:"code"`
	Msg    string `yaml:"msg"`
}

// EnrichmentPayload 语义增强载荷。
type EnrichmentPayload struct {
	Summary     string   `yaml:"summary,omitempty"`
	Description string   `yaml:"description,omitempty"`
	Tags        []string `yaml:"tags,omitempty"`
}

// NewFact 构造辅助（static 事实 confidence=1.0）。
func NewFact(kind FactKind, id string, value interface{}, ev ...Evidence) *Fact {
	return &Fact{
		ID: id, Kind: kind, Value: value, Source: SourceStatic,
		Confidence: 1.0, Evidence: ev, Status: "verified",
	}
}

// OpKey operation 稳定主键（设计文档 §4.4）。
func OpKey(method, path string) string {
	return fmt.Sprintf("op:%s:%s", method, path)
}
