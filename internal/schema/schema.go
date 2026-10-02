// Package schema 定义语言无关的 JSON Schema IR：各语言前端（Go 的 typeschema 等）合成它，
// 事实库与编译器只消费它——编译器不依赖任何语言的分析代码。
package schema

// Schema JSON Schema 节点（有序输出由编译器保证）。
type Schema struct {
	Type             string // object|array|string|integer|number|boolean|null
	Format           string // int64|int32|double|date-time
	Ref              string // $ref 名（命名类型引用）
	Items            *Schema
	AllOf            []*Schema
	OneOf            []*Schema
	AnyOf            []*Schema
	Not              *Schema
	Props            []Prop // object 属性（有序）
	Required         []string
	Enum             []string // 枚举值（字符串化）
	Additional       bool     // additionalProperties: true
	Nullable         bool     // 3.1: type 数组含 null
	ContentEncoding  string   // JSON Schema contentEncoding
	ContentMediaType string   // JSON Schema contentMediaType
	Description      string
	Unknown          bool // x-specforge-unknown
	UnknownWhy       string
	EnumSource       string // 枚举证据符号
	Min, Max         *float64
	MinLen, MaxLen   *int
	MinItems         *int // 数组最少元素数（validator min/len 作用于 slice）
	MaxItems         *int // 数组最多元素数
	Pattern          string
	Default          any
	Examples         []any
	Discriminator    string
	ReadOnly         bool
	WriteOnly        bool
	NoBody           bool // RawMessage 透传: 无 schema
}

// Prop object 的一个属性。
type Prop struct {
	Name      string
	Schema    *Schema
	Required  bool
	OmitEmpty bool
}
