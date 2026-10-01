package compiler

import (
	"fmt"
	"strings"

	"github.com/specforge/specforge/internal/schema"
)

// 确定性 YAML 渲染器（设计文档 §8.5 序列化规范）:
//
//      1. 固定 key 顺序（OpenAPI 规范推荐序，非字典序）
//      2. 缩进 2 空格，不用流式风格
//      3. 引号仅在需要时添加，单引号风格
//      4. 数值定点输出; 整数绝不出现 .0
//      5. 不折行; 文件尾单个换行
//      6. 空值禁止: 显式 x-specforge-unknown 或省略

// render 输出 OpenAPI 3.1 YAML。
func render(d *Document) ([]byte, error) {
	var b strings.Builder
	w := func(format string, args ...interface{}) {
		fmt.Fprintf(&b, format, args...)
	}

	w("openapi: 3.1.0\n")
	w("info:\n")
	w("  title: %s\n", quoteIfNeeded(d.Title))
	w("  version: %s\n", quoteIfNeeded(d.Version))
	if d.Description != "" {
		w("  description: %s\n", quoteIfNeeded(d.Description))
	}

	// securitySchemes 在 components 下输出; 先写 tags（operation 标签汇总）
	tagSet := map[string]bool{}
	var tags []string
	for _, op := range d.Operations {
		for _, t := range op.Tags {
			if !tagSet[t] {
				tagSet[t] = true
				tags = append(tags, t)
			}
		}
	}
	if len(tags) > 0 {
		w("tags:\n")
		for _, t := range tags {
			w("  - name: %s\n", quoteIfNeeded(t))
		}
	}

	w("paths:\n")
	currentPath := ""
	for _, op := range d.Operations {
		if op.Path != currentPath {
			currentPath = op.Path
			w("  %s:\n", quoteIfNeeded(op.Path))
		}
		writeOperation(w, &b, op, 4)
	}

	// components
	if len(d.Schemas) > 0 || len(d.SecSchemes) > 0 {
		w("components:\n")
		if len(d.SecSchemes) > 0 {
			w("  securitySchemes:\n")
			for _, s := range d.SecSchemes {
				w("    %s:\n", quoteIfNeeded(s.Name))
				w("      type: %s\n", s.Type)
				if s.Type == "apiKey" {
					w("      name: %s\n", quoteIfNeeded(s.HeaderName))
					w("      in: header\n")
				}
				if s.Type == "http" {
					w("      scheme: bearer\n")
				}
			}
		}
		if len(d.Schemas) > 0 {
			w("  schemas:\n")
			for _, ns := range d.Schemas {
				w("    %s:\n", quoteIfNeeded(ns.Name))
				writeSchema(w, &b, ns.Schema, 6, "      ")
			}
		}
	}
	return []byte(b.String()), nil
}

var methodOrderList = map[string]int{}

func writeOperation(w func(string, ...interface{}), b *strings.Builder, op Operation, indent int) {
	pad := strings.Repeat(" ", indent)
	w("%s%s:\n", pad, strings.ToLower(op.Method))
	p2 := pad + "  "
	if len(op.Tags) > 0 {
		w("%stags:\n", p2)
		for _, t := range op.Tags {
			w("%s  - %s\n", p2, quoteIfNeeded(t))
		}
	}
	if op.Summary != "" {
		w("%ssummary: %s\n", p2, quoteIfNeeded(op.Summary))
	}
	if op.Description != "" {
		w("%sdescription: %s\n", p2, quoteIfNeeded(op.Description))
	}
	w("%soperationId: %s\n", p2, quoteIfNeeded(op.OperationID))
	if len(op.Params) > 0 {
		w("%sparameters:\n", p2)
		for _, p := range op.Params {
			w("%s  - name: %s\n", p2, quoteIfNeeded(p.Name))
			w("%s    in: %s\n", p2, p.In)
			if p.Required {
				w("%s    required: true\n", p2)
			}
			if p.Description != "" {
				w("%s    description: %s\n", p2, quoteIfNeeded(p.Description))
			} else if p.Origin != "" {
				w("%s    description: %s\n", p2, quoteIfNeeded("origin: "+p.Origin))
			}
			w("%s    schema:\n", p2)
			p3 := p2 + "      "
			w("%stype: %s\n", p3, p.Type)
			if p.Format != "" {
				w("%sformat: %s\n", p3, p.Format)
			}
			if p.Items != "" {
				w("%sitems:\n", p3)
				w("%s  type: %s\n", p3, p.Items)
			}
			if len(p.Enum) > 0 {
				w("%senum:\n", p3)
				for _, e := range p.Enum {
					w("%s  - %s\n", p3, quoteIfNeeded(e))
				}
			}
		}
	}
	if op.Body != nil {
		w("%srequestBody:\n", p2)
		w("%s  required: true\n", p2)
		w("%s  content:\n", p2)
		w("%s    application/json:\n", p2)
		w("%s      schema:\n", p2)
		if op.Body.EnvelopeName != "" && op.Body.DataField != "" {
			// 请求信封：$ref 信封 + allOf 把承载字段收窄为业务体（与响应信封同形）。
			p6 := p2 + "        "
			w("%sallOf:\n", p6)
			w("%s  - $ref: '#/components/schemas/%s'\n", p6, escapeRef(op.Body.EnvelopeName))
			w("%s  - type: object\n", p6)
			w("%s    properties:\n", p6)
			w("%s      %s:\n", p6, quoteIfNeeded(op.Body.DataField))
			w("%s        $ref: '#/components/schemas/%s'\n", p6, escapeRef(op.Body.SchemaName))
			w("%s    required:\n", p6)
			w("%s      - %s\n", p6, quoteIfNeeded(op.Body.DataField))
		} else {
			w("%s        $ref: '#/components/schemas/%s'\n", p2, escapeRef(op.Body.SchemaName))
		}
	}
	if len(op.Responses) > 0 {
		w("%sresponses:\n", p2)
		p3 := p2 + "  "
		for _, r := range op.Responses {
			w("%s'%s':\n", p3, r.Status)
			p4 := p3 + "    "
			w("%sdescription: %s\n", p4, quoteIfNeeded(r.Description))
			if r.Raw || r.EnvelopeName != "" {
				writeDiscoveredBody(w, p4, r)
			} else if r.HasBody && r.SchemaName != "" || len(r.Codes) > 0 {
				w("%scontent:\n", p4)
				w("%s  application/json:\n", p4)
				w("%s    schema:\n", p4)
				// 信封包装: code / msg / data（设计文档 §3.3 response_envelope）
				w("%s      type: object\n", p4)
				w("%s      properties:\n", p4)
				w("%s        code:\n", p4)
				w("%s          type: integer\n", p4)
				if len(r.Codes) > 0 {
					w("%s          enum:\n", p4)
					for _, c := range r.Codes {
						w("%s            - %d\n", p4, c)
					}
				}
				w("%s        msg:\n", p4)
				w("%s          type: string\n", p4)
				if r.HasBody && r.ArrayElem != "" {
					w("%s        data:\n", p4)
					w("%s          type: array\n", p4)
					w("%s          items:\n", p4)
					w("%s            $ref: '#/components/schemas/%s'\n", p4, escapeRef(r.ArrayElem))
				} else if r.HasBody && r.SchemaName != "" {
					w("%s        data:\n", p4)
					w("%s          $ref: '#/components/schemas/%s'\n", p4, escapeRef(r.SchemaName))
				} else if r.HasBody && r.MapValueType != "" {
					w("%s        data:\n", p4)
					w("%s          type: object\n", p4)
					w("%s          additionalProperties:\n", p4)
					w("%s            type: %s\n", p4, r.MapValueType)
				}
			}
			if len(r.Sinks) > 0 {
				w("%sx-specforge-sinks:\n", p4)
				for _, sk := range r.Sinks {
					w("%s  - %s\n", p4, quoteIfNeeded(sk))
				}
			}
			if r.HasUnresolved {
				w("%sx-specforge-unknown: 'error envelope code unresolved'\n", p4)
			}
		}
	}
	if len(op.Security) > 0 {
		w("%ssecurity:\n", p2)
		for _, s := range op.Security {
			w("%s  - %s: []\n", p2, quoteIfNeeded(s))
		}
	}
	// x-specforge 扩展（固定键序: confidence, evidence, unknown）
	if op.Confidence > 0 && op.Confidence < 0.95 {
		w("%sx-specforge-confidence: %s\n", p2, fixed2(op.Confidence))
	}
	if len(op.Evidence) > 0 {
		w("%sx-specforge-evidence:\n", p2)
		for _, e := range op.Evidence {
			w("%s  - %s\n", p2, quoteIfNeeded(e))
		}
	}
	if len(op.Unknowns) > 0 {
		w("%sx-specforge-unknown:\n", p2)
		for _, u := range op.Unknowns {
			w("%s  - %s\n", p2, quoteIfNeeded(u))
		}
	}
}

func writeSchema(w func(string, ...interface{}), b *strings.Builder, sc *schema.Schema, indent int, pad string) {
	if sc == nil {
		w("%sdescription: 'unresolved schema'\n", pad)
		return
	}
	if sc.NoBody {
		w("%sdescription: 'raw passthrough (json.RawMessage)'\n", pad)
		return
	}
	if sc.Ref != "" {
		w("%s$ref: '#/components/schemas/%s'\n", pad, escapeRef(sc.Ref))
	}
	writeSchemaCompositions(w, indent, pad, "allOf", sc.AllOf)
	writeSchemaCompositions(w, indent, pad, "oneOf", sc.OneOf)
	writeSchemaCompositions(w, indent, pad, "anyOf", sc.AnyOf)
	if sc.Not != nil {
		w("%snot:\n", pad)
		writeSchema(w, b, sc.Not, indent+2, pad+"  ")
	}
	if sc.Type != "" {
		if sc.Nullable {
			// 3.1 可空: type 数组（§7.2.6），唯一 type 键
			w("%stype: [%s, 'null']\n", pad, sc.Type)
		} else {
			w("%stype: %s\n", pad, sc.Type)
		}
	}
	if sc.Format != "" && !sc.Nullable {
		w("%sformat: %s\n", pad, sc.Format)
	}
	if sc.Description != "" {
		w("%sdescription: %s\n", pad, quoteIfNeeded(sc.Description))
	}
	if sc.Default != nil {
		writeSchemaValue(w, pad, "default", sc.Default)
	}
	if len(sc.Examples) > 0 {
		w("%sexamples:\n", pad)
		for _, example := range sc.Examples {
			writeSchemaSequenceValue(w, pad+"  ", example)
		}
	}
	if sc.Discriminator != "" {
		w("%sdiscriminator:\n", pad)
		w("%s  propertyName: %s\n", pad, quoteIfNeeded(sc.Discriminator))
	}
	if sc.ReadOnly {
		w("%sreadOnly: true\n", pad)
	}
	if sc.WriteOnly {
		w("%swriteOnly: true\n", pad)
	}
	if len(sc.Enum) > 0 {
		w("%senum:\n", pad)
		for _, e := range sc.Enum {
			w("%s  - %s\n", pad, quoteIfNeeded(e))
		}
	}
	if sc.Min != nil {
		w("%sminimum: %s\n", pad, numStr(*sc.Min))
	}
	if sc.Max != nil {
		w("%smaximum: %s\n", pad, numStr(*sc.Max))
	}
	if sc.MinLen != nil {
		w("%sminLength: %d\n", pad, *sc.MinLen)
	}
	if sc.MaxLen != nil {
		w("%smaxLength: %d\n", pad, *sc.MaxLen)
	}
	if sc.MinItems != nil {
		w("%sminItems: %d\n", pad, *sc.MinItems)
	}
	if sc.MaxItems != nil {
		w("%smaxItems: %d\n", pad, *sc.MaxItems)
	}
	if sc.Items != nil {
		w("%sitems:\n", pad)
		writeSchema(w, b, sc.Items, indent+2, pad+"  ")
	}
	if sc.Additional {
		w("%sadditionalProperties: true\n", pad)
	}
	if len(sc.Props) > 0 {
		w("%sproperties:\n", pad)
		for _, p := range sc.Props {
			w("%s  %s:\n", pad, quoteIfNeeded(p.Name))
			writeSchema(w, b, p.Schema, indent+2, pad+"    ")
		}
	}
	if len(sc.Required) > 0 {
		w("%srequired:\n", pad)
		for _, r := range sc.Required {
			w("%s  - %s\n", pad, quoteIfNeeded(r))
		}
	}
	if sc.Unknown {
		w("%sx-specforge-unknown: %s\n", pad, quoteIfNeeded(sc.UnknownWhy))
	}
}

func writeSchemaCompositions(w func(string, ...interface{}), indent int, pad, key string, variants []*schema.Schema) {
	if len(variants) == 0 {
		return
	}
	w("%s%s:\n", pad, key)
	for _, variant := range variants {
		var buf strings.Builder
		writeSchema(func(format string, args ...interface{}) { fmt.Fprintf(&buf, format, args...) }, &buf, variant, indent+2, pad+"  ")
		lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
		if len(lines) == 0 || lines[0] == "" {
			continue
		}
		w("%s  - %s\n", pad, strings.TrimPrefix(lines[0], pad+"  "))
		for _, line := range lines[1:] {
			w("%s    %s\n", pad, strings.TrimPrefix(line, pad+"  "))
		}
	}
}

func writeSchemaValue(w func(string, ...interface{}), pad, key string, value any) {
	switch v := value.(type) {
	case string:
		w("%s%s: %s\n", pad, key, quoteIfNeeded(v))
	case bool:
		w("%s%s: %t\n", pad, key, v)
	case int:
		w("%s%s: %d\n", pad, key, v)
	case int64:
		w("%s%s: %d\n", pad, key, v)
	case float64:
		w("%s%s: %s\n", pad, key, numStr(v))
	default:
		w("%s%s: %s\n", pad, key, quoteIfNeeded(fmt.Sprint(v)))
	}
}

func writeSchemaSequenceValue(w func(string, ...interface{}), pad string, value any) {
	switch v := value.(type) {
	case string:
		w("%s- %s\n", pad, quoteIfNeeded(v))
	case bool:
		w("%s- %t\n", pad, v)
	case int:
		w("%s- %d\n", pad, v)
	case int64:
		w("%s- %d\n", pad, v)
	case float64:
		w("%s- %s\n", pad, numStr(v))
	default:
		w("%s- %s\n", pad, quoteIfNeeded(fmt.Sprint(v)))
	}
}

// schemaView 别名已移除，直接使用 schema.Schema。

var _ = fmt.Sprintf

func numStr(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}

func fixed2(f float64) string { return fmt.Sprintf("%.2f", f) }

func escapeRef(s string) string { return s }

// quoteIfNeeded YAML 单引号转义（保守规则）。
func quoteIfNeeded(s string) string {
	if s == "" {
		return "''"
	}
	if needsQuote(s) {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return s
}

func needsQuote(s string) bool {
	// 开头特殊字符
	c := s[0]
	if c == '&' || c == '*' || c == '?' || c == '|' || c == '-' || c == '<' ||
		c == '>' || c == '=' || c == '!' || c == '%' || c == '@' || c == '`' ||
		c == '{' || c == '[' || c == ']' || c == '}' || c == ',' || c == '#' ||
		c == '\'' || c == '"' || c == ' ' {
		return true
	}
	if s[len(s)-1] == ' ' || s[len(s)-1] == ':' {
		return true
	}
	if strings.Contains(s, ": ") || strings.Contains(s, " #") {
		return true
	}
	if strings.Contains(s, "\n") || strings.Contains(s, "\t") {
		return true
	}
	// 布尔/空/null 字面量形态
	switch strings.ToLower(s) {
	case "true", "false", "null", "yes", "no", "on", "off", "~":
		return true
	}
	// 数字形态
	if isNumericLike(s) {
		return true
	}
	return false
}

func isNumericLike(s string) bool {
	dot := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' {
			continue
		}
		if c == '.' && !dot && i > 0 {
			dot = true
			continue
		}
		if (c == '-' || c == '+') && i == 0 {
			continue
		}
		return false
	}
	return len(s) > 0
}

// writeDiscoveredBody 渲染自动发现的响应体：原生写出直接引用业务体；真实信封以 $ref 引用，
// 成功行再用 allOf 把 data 槽收窄为具体类型（与 swag `Envelope{result=T}` 同形）；
// 同一状态码下有多种响应体时渲染为 oneOf。
func writeDiscoveredBody(w func(string, ...interface{}), p4 string, r ResponseOut) {
	variants := r.Variants
	if len(variants) == 0 {
		variants = []BodyVariant{{SchemaName: r.SchemaName, ArrayElem: r.ArrayElem, MapValueType: r.MapValueType,
			EnvelopeName: r.EnvelopeName, DataField: r.DataField, Raw: r.Raw}}
		if !r.HasBody {
			variants[0].SchemaName, variants[0].ArrayElem, variants[0].MapValueType = "", "", ""
		}
	}
	if len(variants) == 1 && variants[0].Raw && !variants[0].hasData() {
		return
	}
	w("%scontent:\n", p4)
	w("%s  application/json:\n", p4)
	w("%s    schema:\n", p4)
	p5 := p4 + "      "
	if len(variants) == 1 {
		writeVariant(w, p5, variants[0])
		return
	}
	w("%soneOf:\n", p5)
	item := p5 + "    "
	for _, v := range variants {
		var buf strings.Builder
		writeVariant(func(f string, a ...interface{}) { fmt.Fprintf(&buf, f, a...) }, item, v)
		w("%s", p5+"  - "+strings.TrimPrefix(buf.String(), item))
	}
}

// writeVariant 渲染一种响应体形态的 schema（pad 为 schema 节点的缩进）。
func writeVariant(w func(string, ...interface{}), p5 string, v BodyVariant) {
	switch {
	case v.Raw:
		writeDataSchema(w, p5, v)
	case v.hasData() && v.DataField != "":
		w("%sallOf:\n", p5)
		w("%s  - $ref: '#/components/schemas/%s'\n", p5, escapeRef(v.EnvelopeName))
		w("%s  - type: object\n", p5)
		w("%s    properties:\n", p5)
		w("%s      %s:\n", p5, quoteIfNeeded(v.DataField))
		writeDataSchema(w, p5+"        ", v)
	default:
		w("%s$ref: '#/components/schemas/%s'\n", p5, escapeRef(v.EnvelopeName))
	}
}

// writeDataSchema 业务体 schema：切片 → array+items，map → additionalProperties，其余 → $ref。
func writeDataSchema(w func(string, ...interface{}), p string, r BodyVariant) {
	switch {
	case r.ArrayElem != "":
		w("%stype: array\n", p)
		w("%sitems:\n", p)
		w("%s  $ref: '#/components/schemas/%s'\n", p, escapeRef(r.ArrayElem))
	case r.SchemaName != "":
		w("%s$ref: '#/components/schemas/%s'\n", p, escapeRef(r.SchemaName))
	case r.MapValueType != "":
		w("%stype: object\n", p)
		w("%sadditionalProperties:\n", p)
		w("%s  type: %s\n", p, r.MapValueType)
	}
}
