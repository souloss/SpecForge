// Package facts 实现 API Fact Graph 数据模型（设计文档 §4）。
package facts

import (
	"fmt"

	"github.com/specforge/specforge/internal/schema"
)

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
	ID         string     `yaml:"id"`         // 稳定主键（contract:op:METHOD:/path、schema:<类型 ID> 等）
	Kind       FactKind   `yaml:"kind"`       // 事实类型（与 Value 的载荷类型一致）
	Value      Payload    `yaml:"value"`      // 强类型载荷（封闭集合）
	Source     FactSource `yaml:"source"`     // 来源：static/llm/runtime/human
	Confidence float64    `yaml:"confidence"` // 置信度 0~1
	Evidence   []Evidence `yaml:"evidence"`   // 证据（无有效证据的事实在编译前被丢弃）
	Status     string     `yaml:"status"`     // 生命周期状态
	// Verification LLM 事实的核对强度（VerifyTyped/VerifySymbol/VerifyText）；静态事实为空。
	Verification string `yaml:"verification,omitempty"`
}

// Payload 事实载荷的封闭集合：只有本包定义的载荷类型实现它（新增事实类型 = 在此新增载荷）。
type Payload interface {
	payloadKind() FactKind
}

// SchemaPayload schema 事实载荷（语言无关 schema IR）。
type SchemaPayload struct {
	Schema *schema.Schema `yaml:"schema"` // 合成的 schema
}

func (RoutePayload) payloadKind() FactKind        { return KindRoute }
func (ContractPayload) payloadKind() FactKind     { return KindContract }
func (SchemaPayload) payloadKind() FactKind       { return KindSchema }
func (SecurityPayload) payloadKind() FactKind     { return KindSecurity }
func (ErrorCatalogPayload) payloadKind() FactKind { return KindErrorCatalog }
func (EnrichmentPayload) payloadKind() FactKind   { return KindEnrichment }

// Contract contract 载荷（非 contract 事实返回 false）。
func (f *Fact) Contract() (ContractPayload, bool) {
	cp, ok := f.Value.(ContractPayload)
	return cp, ok
}

// Schema schema 载荷（非 schema 事实返回 false）。
func (f *Fact) Schema() (*schema.Schema, bool) {
	sp, ok := f.Value.(SchemaPayload)
	return sp.Schema, ok && sp.Schema != nil
}

// Enrichment 语义增强载荷。
func (f *Fact) Enrichment() (EnrichmentPayload, bool) {
	ep, ok := f.Value.(EnrichmentPayload)
	return ep, ok
}

// Security security 载荷。
func (f *Fact) Security() (SecurityPayload, bool) {
	sp, ok := f.Value.(SecurityPayload)
	return sp, ok
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
	// UnsummarizedWrappers 响应体取自形参却没被识别为包装器的函数（符号 ID）：L2 包装器摘要兜底的输入。
	UnsummarizedWrappers []string `yaml:"unsummarizedWrappers,omitempty"`
	// ErrCandidates 切片内已出现的具体错误码候选（证据注入）：err 变量兜底时
	// 作为 LLM 的候选目录，帮助其缩小选码范围而非在全量目录里空猜。
	ErrCandidates []ErrCandidateFact `yaml:"errCandidates,omitempty"`
	// DataCandidates any 响应兜底的字段候选（证据注入）：切片内已出现的字段名
	// 作为 LLM 候选，帮助其收窄 any 响应结构而非从空证据里编造。
	DataCandidates []DataCandidateFact `yaml:"dataCandidates,omitempty"`
}

// ErrCandidateFact 切片内错误码候选（供 LLM 兜底选码）。
type ErrCandidateFact struct {
	Symbol string `yaml:"symbol"` // 全限定常量符号
	Name   string `yaml:"name"`   // 末段常量名（跨包匹配键）
	Code   int    `yaml:"code"`   // 码值（-1 表示未解析）
}

// DataCandidateFact any 响应兜底的字段候选（供 LLM 收窄结构）。
type DataCandidateFact struct {
	Name string `yaml:"name"` // 字段名（JSON 键）
}

// ParamFact 参数事实。
type ParamFact struct {
	In          string   `yaml:"in"`                    // query/path/header/cookie
	Name        string   `yaml:"name"`                  // 参数名（绑定 tag 名或字面量 key）
	Required    bool     `yaml:"required"`              // 是否必填（path 恒为 true）
	Type        string   `yaml:"type"`                  // OAS 基础类型
	Format      string   `yaml:"format,omitempty"`      // OAS format
	Items       string   `yaml:"items,omitempty"`       // type=array 时的元素基础类型
	Enum        []string `yaml:"enum,omitempty"`        // 枚举取值（oneof 约束或枚举常量）
	Description string   `yaml:"description,omitempty"` // 字段 doc 注释
	Origin      string   `yaml:"origin,omitempty"`      // 来源证据（如 fiber:QueryParser）
}

// BodyFact 请求体事实。
type BodyFact struct {
	ContentType string `yaml:"contentType"` // 媒体类型，如 application/json
	SchemaType  string `yaml:"schemaType"`  // 业务请求体类型 ID
	// EnvelopeType 请求信封结构体类型 ID（绑定包装器把业务体装进信封再解析）；空 = 业务体即请求体。
	EnvelopeType string `yaml:"envelopeType,omitempty"`
	// DataField 请求信封中承载业务体的字段 JSON 名（编译期 allOf 收窄为 SchemaType）。
	DataField string `yaml:"dataField,omitempty"`
	// Origin 绑定来源证据（fiber:BodyParser 或绑定包装器符号）。
	Origin string `yaml:"origin,omitempty"`
}

// ResponseFact 响应矩阵行（status × envelope × schema，设计文档 §4.2）。
type ResponseFact struct {
	Status     int       `yaml:"status"`
	Envelope   *Envelope `yaml:"envelope,omitempty"`
	SchemaType string    `yaml:"schemaType,omitempty"`
	HasBody    bool      `yaml:"hasBody"`
	Sink       string    `yaml:"sink"`
	// Source 该行的事实来源：空 = static；"llm" = 由 LLM 兜底采集补齐。
	Source string `yaml:"source,omitempty"`
	// ErrSource 错误码未静态解析时，err 变量的来源证据（如
	// "errgroup.Wait@ipoServer.go:233"），供 LLM 兜底定位码归因依据。
	ErrSource string `yaml:"errSource,omitempty"`
	// MapValueType data 槽为 map[K]V 且 V 为基础类型时，V 的 JSON schema 类型
	// （integer/string/...）；空 = 非 map 或值不可定型。用于编译 additionalProperties map。
	MapValueType string `yaml:"mapValueType,omitempty"`
	// EnvelopeType 真实信封结构体类型 ID（包装器自动发现）；空 = profile 信封（code/msg/data）或无信封。
	EnvelopeType string `yaml:"envelopeType,omitempty"`
	// DataField 信封中承载 data 的字段 JSON 名（成功行用 allOf 将其收窄为具体 schema）。
	DataField string `yaml:"dataField,omitempty"`
	// Raw 原生写出：SchemaType 即响应体本身，无业务信封。
	Raw bool `yaml:"raw,omitempty"`
	// Failure 原生写出位于错误分支（if err != nil）：同状态码下与成功体并列为 oneOf 的一支。
	Failure bool `yaml:"failure,omitempty"`
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

// NewFact 构造辅助（static 事实 confidence=1.0；Kind 取自载荷类型，二者不会不一致）。
func NewFact(id string, value Payload, ev ...Evidence) *Fact {
	return &Fact{
		ID: id, Kind: value.payloadKind(), Value: value, Source: SourceStatic,
		Confidence: 1.0, Evidence: ev, Status: "verified",
	}
}

// OpKey operation 稳定主键（设计文档 §4.4）。
func OpKey(method, path string) string {
	return fmt.Sprintf("op:%s:%s", method, path)
}
