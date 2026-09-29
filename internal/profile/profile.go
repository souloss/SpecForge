// Package profile 约定画像（设计文档 §3.3）——最大成本杠杆。
//
// P0/P1 阶段 profile 可人工提供（设计文档 §12.6 失败回退路径:
// 「画像不等 P3，先人工写一份」），结构与自动学习输出完全一致。
package profile

import (
	"os"

	"gopkg.in/yaml.v3"
)

// Profile 仓库约定画像。
type Profile struct {
	Framework        string                     `yaml:"framework"`
	ResponseSinks    []SinkPattern              `yaml:"response_sinks"`
	ResponseEnvelope *EnvelopeSpec              `yaml:"response_envelope"`
	RequestBinding   []BindingRule              `yaml:"request_binding"`
	AuthMiddleware   map[string]SecurityMapping `yaml:"auth_middleware"`
	PathConventions  PathConventions            `yaml:"path_conventions"`
	ErrorCodesSource string                     `yaml:"error_codes_source"`
	ResponseMutators []string                   `yaml:"response_mutating_middleware,omitempty"`
}

// SinkPattern 响应汇聚点模式。
type SinkPattern struct {
	Symbol    string `yaml:"symbol"`    // 全限定函数 ID
	Signature string `yaml:"signature"` // 形参签名（data 槽位判定）
	DataSlot  int    `yaml:"data_slot"` // data 实参下标（0 起，不含接收者）；NoSlot 表示无 data 实参
	ErrSlot   int    `yaml:"err_slot"`  // error 实参下标；NoSlot 表示无 err 实参（恒为成功写出）
	Status    int    `yaml:"status"`    // 写出的 HTTP 状态码

	// 以下由包装器自动发现（函数摘要）填充，手写 profile 可省略
	EnvelopeType string `yaml:"envelope_type,omitempty"` // 真实信封结构体类型 ID；空 = 走 response_envelope 或无信封
	DataField    string `yaml:"data_field,omitempty"`    // 信封中承载 data 的字段 JSON 名
	ErrField     string `yaml:"err_field,omitempty"`     // 信封中承载错误对象的字段 JSON 名
	BranchOnErr  bool   `yaml:"branch_on_err,omitempty"` // 同一调用点按 err 是否为 nil 分流成功/失败信封
	Raw          bool   `yaml:"raw,omitempty"`           // 框架原生写出器：data 即响应体本身，无业务信封
	Auto         bool   `yaml:"-"`                       // 自动发现（非手写），报告中标注来源
	SuccessCode  *int   `yaml:"-"`                       // 成功分支信封里的固定业务码（包装器摘要得出）；nil = 用画像默认
	FailureCode  *int   `yaml:"-"`                       // 失败分支信封里的固定业务码（如 "code": 500）；nil = 由 err 值决定
	// Learned LLM 包装器摘要得出（经复核）：由它产生的响应行置信度按 symbol 级核对封顶。
	Learned bool `yaml:"-"`
	// ErrEnvelopeType 失败分支写出的信封类型（与成功分支不同时，如 BaseResp / ErrResponse）；空 = 同 EnvelopeType。
	ErrEnvelopeType string `yaml:"err_envelope_type,omitempty"`
}

// NoSlot 槽位不存在（SinkPattern.DataSlot/ErrSlot 取值）。
const NoSlot = -1

// EnvelopeSpec 响应信封结构。
type EnvelopeSpec struct {
	Type        string            `yaml:"type"`
	Properties  map[string]string `yaml:"properties"`
	DataSlot    string            `yaml:"data_slot"`
	SuccessCode int               `yaml:"success_code"`
}

// BindingRule 请求绑定模式。
type BindingRule struct {
	Pattern     string `yaml:"pattern"`
	ContentType string `yaml:"contentType"`
}

// SecurityMapping 中间件 → security 语义。
type SecurityMapping struct {
	Header    string `yaml:"header"`
	Scheme    string `yaml:"scheme"`
	Required  bool   `yaml:"required"`
	ScopeArg  int    `yaml:"scope_arg,omitempty"`
	ParamName string `yaml:"param_name,omitempty"`
}

// PathConventions 路径约定。
type PathConventions struct {
	WildcardExpansion map[string][]string `yaml:"wildcard_expansion"`
}

// Load 从 YAML 文件加载画像。
func Load(path string) (*Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Default(), nil // 无画像 → 默认画像（宽松模式）
	}
	var p Profile
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Default 默认画像: 无约定知识，静态抽取按通用规则。
func Default() *Profile {
	return &Profile{
		Framework: "unknown",
		PathConventions: PathConventions{
			WildcardExpansion: map[string][]string{},
		},
		AuthMiddleware: map[string]SecurityMapping{},
	}
}

// IsSink 判定符号是否为响应汇聚点。
func (p *Profile) IsSink(symbolID string) (SinkPattern, bool) {
	for _, s := range p.ResponseSinks {
		if matchSymbol(s.Symbol, symbolID) {
			return s, true
		}
	}
	return SinkPattern{}, false
}

// matchSymbol 支持 末段匹配（跨模块路径对齐）。
func matchSymbol(pattern, symbol string) bool {
	if pattern == symbol {
		return true
	}
	// 末段匹配: "code.WriteResponse" 匹配 "trade/pkg/code.WriteResponse"
	patSeg := lastDotSeg(pattern)
	symSeg := lastDotSeg(symbol)
	if patSeg != "" && patSeg == symSeg {
		// 前缀部分需是路径后缀关系
		patPre := pattern[:len(pattern)-len(patSeg)-1]
		symPre := symbol[:len(symbol)-len(symSeg)-1]
		return stringsHasSuffixFold(symPre, patPre)
	}
	return false
}

func lastDotSeg(s string) string {
	// 形如 "pkg.(*Recv).Method" 取 "Method" 的近似: 取最后一个点后段
	// 但方法 ID 含 "(*Recv).Method"——匹配以 ".WriteResponse" 结尾
	if i := lastIndexDot(s); i >= 0 {
		return s[i+1:]
	}
	return s
}

func lastIndexDot(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

func stringsHasSuffixFold(s, suffix string) bool {
	if suffix == "" {
		return true
	}
	if len(s) < len(suffix) {
		return false
	}
	return s[len(s)-len(suffix):] == suffix || hasSegSuffix(s, suffix)
}

// hasSegSuffix "trade/pkg/code" has suffix "pkg/code" 或 "code"。
func hasSegSuffix(s, suffix string) bool {
	return len(s) >= len(suffix)+1 && s[len(s)-len(suffix)-1] == '/' &&
		s[len(s)-len(suffix):] == suffix
}
