// Package compiler 确定性编译器: Fact Graph → OpenAPI 3.1
// （设计文档 §8 八阶段流水线 + §8.5 序列化规范）。
//
// 核心承诺: 相同输入 → 逐字节相同的 YAML。
package compiler

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/schema"
)

// Input 编译输入。
type Input struct {
	ServiceName string
	Framework   string
	Facts       []*facts.Fact
	Schemas     map[string]*schema.Schema
}

// Document 编译产物（结构化中间形态，供渲染器消费）。
type Document struct {
	Title       string
	Version     string
	Description string
	Framework   string
	Operations  []Operation // 稳定排序
	Schemas     []NamedSchema
	SecSchemes  []SecScheme
}

// Operation 一个 operation 的最终形态（渲染器输入，也是 operations.json / explain --json 的机器契约）。
type Operation struct {
	Method      string        `json:"method"`       // HTTP 方法（大写）
	Path        string        `json:"path"`         // OpenAPI 路径模板
	OperationID string        `json:"operation_id"` // operationId
	Summary     string        `json:"summary"`      // 一行摘要（godoc 或 LLM 增强）
	Description string        `json:"description"`  // 补充描述
	Tags        []string      `json:"tags"`         // 分组标签
	Params      []ParamOut    `json:"params"`       // 参数（稳定排序）
	Body        *BodyOut      `json:"body"`         // 请求体；nil = 无
	Responses   []ResponseOut `json:"responses"`    // 响应（按状态码排序）
	Security    []string      `json:"security"`     // 安全方案名
	Confidence  float64       `json:"confidence"`   // 置信度 0~1
	Unknowns    []string      `json:"unknowns"`     // 未解析项（缺口）
	Evidence    []string      `json:"evidence"`     // 证据「标签 file:line」
	Stale       bool          `json:"stale"`        // 证据已过期（保留字段）
}

// ParamOut 输出参数。
type ParamOut struct {
	Name        string   `json:"name"`        // 参数名
	In          string   `json:"in"`          // query/path/header/cookie
	Required    bool     `json:"required"`    // 是否必填
	Type        string   `json:"type"`        // OAS 基础类型
	Format      string   `json:"format"`      // OAS format
	Items       string   `json:"items"`       // type=array 时的元素基础类型
	Enum        []string `json:"enum"`        // 枚举取值
	Description string   `json:"description"` // 字段 doc 注释
	Origin      string   `json:"origin"`      // 来源证据（如 fiber:Query、route-template）
}

// BodyOut 请求体。
type BodyOut struct {
	ContentType  string `json:"content_type"` // 媒体类型
	SchemaName   string `json:"schema"`       // 业务体 $ref 名
	EnvelopeName string `json:"envelope"`     // 请求信封 $ref 名（空 = 无信封）
	DataField    string `json:"data_field"`   // 信封中承载业务体的字段 JSON 名
}

// ResponseOut 响应行（同状态码的多条信封码合并为一行）。
type ResponseOut struct {
	Status        string   `json:"status"`         // HTTP 状态码
	Codes         []int    `json:"codes"`          // 信封码集合（enum）
	HasUnresolved bool     `json:"has_unresolved"` // 是否存在未解析的错误行
	HasDynamic    bool     `json:"has_dynamic"`    // 是否存在业务码取自运行时值的错误行（码集合不封闭）
	HasUncoded    bool     `json:"has_uncoded"`    // 是否存在不带业务码的错误行（错误对象原样序列化）
	Description   string   `json:"description"`    // 聚合描述
	SchemaName    string   `json:"schema"`         // 信封 data 槽的 $ref 名（空 = 无 body）
	ArrayElem     string   `json:"array_elem"`     // 切片响应: data 为 array，元素 $ref 名
	HasBody       bool     `json:"has_body"`       // 是否有响应体
	MapValueType  string   `json:"map_value_type"` // map[K]V 响应: data 为 additionalProperties map，值类型
	Sinks         []string `json:"sinks"`          // 写出点 file:line
	EnvelopeName  string   `json:"envelope"`       // 真实信封结构体的 $ref 名（空 = profile 信封 code/msg/data）
	DataField     string   `json:"data_field"`     // 真实信封中承载业务体的字段 JSON 名（成功行 allOf 收窄）
	Raw           bool     `json:"raw"`            // 原生写出：业务体即响应体，无信封
	// Variants 同一状态码下互不相同的响应体（>1 时渲染为 oneOf；上面的单值字段取第一个变体）。
	Variants []BodyVariant `json:"variants,omitempty"`
}

// BodyVariant 一种响应体形态（信封 + 业务体，或原生业务体）。
type BodyVariant struct {
	SchemaName   string `json:"schema"`         // 业务体 $ref 名
	ArrayElem    string `json:"array_elem"`     // 数组业务体的元素 $ref 名
	MapValueType string `json:"map_value_type"` // map 业务体的值类型
	EnvelopeName string `json:"envelope"`       // 信封 $ref 名
	DataField    string `json:"data_field"`     // 信封中承载业务体的字段
	Raw          bool   `json:"raw"`            // 原生写出（无信封）
}

// addVariant 记录一种响应体形态（去重；既无信封也无业务体的行不产生变体）。
func (g *ResponseOut) addVariant(v BodyVariant) {
	if !v.hasData() && v.EnvelopeName == "" {
		g.Raw = g.Raw || v.Raw
		return
	}
	for _, x := range g.Variants {
		if x == v {
			return
		}
	}
	g.Variants = append(g.Variants, v)
}

// settlePrimary 单值字段取第一个变体（成功行排在前）；只有一个变体时不保留 Variants，渲染与旧口径一致。
func (g *ResponseOut) settlePrimary() {
	if len(g.Variants) == 0 {
		return
	}
	v := g.Variants[0]
	g.SchemaName, g.ArrayElem, g.MapValueType = v.SchemaName, v.ArrayElem, v.MapValueType
	g.EnvelopeName, g.DataField, g.Raw = v.EnvelopeName, v.DataField, g.Raw || v.Raw
	g.HasBody = g.HasBody || v.hasData()
	if len(g.Variants) == 1 {
		g.Variants = nil
	}
}

// hasData 变体是否带业务体。
func (v BodyVariant) hasData() bool {
	return v.SchemaName != "" || v.ArrayElem != "" || v.MapValueType != ""
}

// NamedSchema 命名 schema。
type NamedSchema struct {
	Name   string
	TypeID string
	Schema *schema.Schema
	Shape  string // shapeHash（去重分组键）
}

// SecScheme 安全方案。
type SecScheme struct {
	Name       string
	Type       string
	In         string
	HeaderName string
	Required   bool
	ScopeArg   string
}

// Compile 执行编译流水线（Stage 3–7 的 P0 形态）。
func Compile(in Input) (*Document, error) {
	d := &Document{
		Title:     in.ServiceName + " API",
		Version:   "1.0.0",
		Framework: in.Framework,
		Description: "Generated by specforge from source code. " +
			"Every field is evidence-traceable; unresolved items are marked x-specforge-unknown.",
	}

	// ---- Stage 3: 事实合并（P0 单来源 static，直接索引化） ----
	contractByOp := map[string]*facts.ContractPayload{}
	enrichByOp := map[string]facts.EnrichmentPayload{}
	var secFacts []*facts.SecurityPayload
	var schemaFacts map[string]*schema.Schema = in.Schemas
	for _, f := range in.Facts {
		switch f.Kind {
		case facts.KindContract:
			if cp, ok := f.Contract(); ok {
				contractByOp[f.ID] = &cp
			}
		case facts.KindEnrichment:
			if ep, ok := f.Enrichment(); ok {
				enrichByOp[strings.TrimSuffix(f.ID, ":enrich")] = ep
			}
		case facts.KindSecurity:
			if sp, ok := f.Security(); ok {
				spc := sp
				secFacts = append(secFacts, &spc)
			}
		case facts.KindSchema:
			if s, ok := f.Schema(); ok {
				if schemaFacts == nil {
					schemaFacts = map[string]*schema.Schema{}
				}
				schemaFacts[strings.TrimPrefix(f.ID, "schema:")] = s
			}
		}
	}

	// ---- Stage 4: Schema 图（$ref 命名 + 去重） ----
	var refOf map[string]string
	d.Schemas, refOf = nameAndDedupe(in, schemaFacts)

	// ---- Stage 5: operation 组装 ----
	factByID := make(map[string]*facts.Fact, len(in.Facts)) // 按 ID 索引（避免逐 operation 扫描全部事实）
	for _, f := range in.Facts {
		if _, dup := factByID[f.ID]; !dup {
			factByID[f.ID] = f
		}
	}
	for opKey, cp := range contractByOp {
		method, path := splitOpKey(opKey)
		op := Operation{
			Method: method, Path: path,
			OperationID: cp.OperationID,
			Tags:        cp.Tags,
			Unknowns:    append([]string(nil), cp.Gaps...), // 静态缺口（报告逐 operation 展示）
		}
		if ep, ok := enrichByOp[strings.TrimPrefix(opKey, "contract:")]; ok {
			op.Summary = ep.Summary
			op.Description = ep.Description
		}
		// 参数（稳定排序: in 权重 + name）
		for _, p := range cp.Params {
			op.Params = append(op.Params, ParamOut{
				Name: p.Name, In: p.In, Required: p.Required,
				Type: p.Type, Format: p.Format, Items: p.Items, Enum: p.Enum,
				Description: p.Description, Origin: p.Origin,
			})
		}
		sortParams(op.Params)
		// 请求体
		if cp.RequestBody != nil {
			if ref, ok := refOf[cp.RequestBody.SchemaType]; ok {
				op.Body = &BodyOut{ContentType: cp.RequestBody.ContentType, SchemaName: ref}
				if envRef, ok := refOf[cp.RequestBody.EnvelopeType]; ok && cp.RequestBody.EnvelopeType != "" {
					op.Body.EnvelopeName, op.Body.DataField = envRef, cp.RequestBody.DataField
				}
			} else {
				op.Unknowns = append(op.Unknowns, "request schema not synthesized: "+cp.RequestBody.SchemaType)
			}
		}
		// 响应矩阵 → 按 status 分组合并（OpenAPI responses 是 status 唯一映射，
		// 信封码集合进 code enum —— 设计文档 §4.2 的编译形态）
		respGroups := map[string]*ResponseOut{}
		var respOrder []string
		for _, r := range cp.Responses {
			st := fmt.Sprintf("%d", r.Status)
			g := respGroups[st]
			if g == nil {
				g = &ResponseOut{Status: st, HasBody: r.HasBody}
				respGroups[st] = g
				respOrder = append(respOrder, st)
			}
			code, msg := -1, ""
			if r.Envelope != nil {
				code, msg = r.Envelope.Code, r.Envelope.Msg
			}
			if r.Raw {
				// 原生写出（无业务信封）：信封码无意义，描述取 HTTP 状态文本（错误分支注明）。
				desc := statusText(r.Status)
				if r.Failure {
					desc += errorBranchSuffix
				}
				g.Description = appendDesc(g.Description, desc)
				code = rawRowCode
			} else if code == 0 {
				g.Description = appendDesc(g.Description, "Success (code=0)")
			} else if code > 0 {
				g.Description = appendDesc(g.Description,
					fmt.Sprintf("Business error (code=%d): %s", code, msg))
			} else if code == facts.DynamicCode {
				g.HasDynamic = true
				g.Description = appendDesc(g.Description, "Business error (code passed through at runtime from "+sitesOf(r)+")")
			} else if code == facts.UncodedCode {
				g.HasUncoded = true
				g.Description = appendDesc(g.Description, "Error without business code (raised by "+sitesOf(r)+")")
			} else {
				g.HasUnresolved = true
				desc := "Business error (code unresolved from source)"
				if r.Envelope != nil && r.Envelope.Msg != "" {
					desc = "Business error (code unresolved from source; " + r.Envelope.Msg + ")"
				}
				g.Description = appendDesc(g.Description, desc)
			}
			if code >= 0 && !r.Raw {
				g.Codes = appendUniq(g.Codes, code)
			}
			var v BodyVariant
			if r.SchemaType != "" {
				if strings.HasPrefix(r.SchemaType, "[]") {
					// 切片响应: data = array + items $ref
					elem := strings.TrimPrefix(r.SchemaType, "[]")
					if ref, ok := refOf[elem]; ok {
						v.ArrayElem = ref
					} else {
						op.Unknowns = append(op.Unknowns, "response schema not synthesized: "+r.SchemaType)
					}
				} else if ref, ok := refOf[r.SchemaType]; ok {
					v.SchemaName = ref
				} else {
					op.Unknowns = append(op.Unknowns, "response schema not synthesized: "+r.SchemaType)
				}
			}
			v.MapValueType = r.MapValueType
			if ref, ok := refOf[r.EnvelopeType]; ok && r.EnvelopeType != "" {
				v.EnvelopeName = ref
			}
			v.DataField, v.Raw = r.DataField, r.Raw
			g.addVariant(v)
			if r.Sink != "" && r.Sink != "(multiple)" {
				g.Sinks = appendUniqStr(g.Sinks, r.Sink)
			}
		}
		sort.Strings(respOrder)
		for _, st := range respOrder {
			g := respGroups[st]
			g.settlePrimary()
			sort.Ints(g.Codes)
			op.Responses = append(op.Responses, *g)
		}
		// security（来自该 operation 的中间件链，设计文档 F7）
		op.Security = cp.Security
		// 置信度: contract 事实的 confidence（由 engine 计算）
		if f := factByID[opKey]; f != nil {
			op.Confidence = f.Confidence
			for _, ev := range f.Evidence {
				op.Evidence = appendUniqStr(op.Evidence, fmt.Sprintf("%s %s:%d", evidenceLabel(ev), ev.File, ev.StartLine))
			}
		}
		d.Operations = append(d.Operations, op)
	}

	// ---- Stage 7: 规范化排序 ----
	sort.Slice(d.Operations, func(i, j int) bool {
		a, b := d.Operations[i], d.Operations[j]
		if a.Path != b.Path {
			return pathLess(a.Path, b.Path)
		}
		return methodOrder(a.Method) < methodOrder(b.Method)
	})
	sort.Slice(d.Schemas, func(i, j int) bool { return d.Schemas[i].Name < d.Schemas[j].Name })
	sort.Slice(secFacts, func(i, j int) bool {
		return secFacts[i].MiddlewareSymbol < secFacts[j].MiddlewareSymbol
	})
	seenScheme := map[string]bool{}
	for _, sp := range secFacts {
		name := sp.MiddlewareSymbol
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		if sp.Header != "" {
			name = sp.Header
		}
		if seenScheme[name] {
			continue
		}
		seenScheme[name] = true
		d.SecSchemes = append(d.SecSchemes, SecScheme{
			Name: name, Type: schemeTypeOf(sp), In: "header",
			HeaderName: sp.Header, Required: sp.Required, ScopeArg: sp.ScopeArg,
		})
	}
	d.Title = in.ServiceName + " API"
	return d, nil
}

// nameAndDedupe §8.3 $ref 命名：按类型身份（typeID）一一对应 schema，不做结构去重——
// 结构同形但名字/语义不同的类型（如 CreateXxxParams 与 UpdateXxxParams）必须各自保留，
// 否则 operation 会引用到另一个接口的 DTO（错误事实）。
// 命名按 typeID 排序后分配（确定性）：短名 → 包名.短名 → 短名-shapeHash。
// 返回 (NamedSchema 列表, typeID→$ref 名映射)。
func nameAndDedupe(in Input, schemas map[string]*schema.Schema) ([]NamedSchema, map[string]string) {
	ids := make([]string, 0, len(schemas))
	for id := range schemas {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	byName := map[string]bool{}
	refOf := map[string]string{}
	out := make([]NamedSchema, 0, len(ids))
	for _, id := range ids {
		shape := canonicalShape(schemas[id])
		name := componentName(lastSeg(id))
		if byName[name] {
			name = componentName(pkgShort(id) + "." + lastSeg(id))
		}
		if byName[name] {
			name = componentName(lastSeg(id) + "-" + shortHash(id))
		}
		byName[name] = true
		out = append(out, NamedSchema{Name: name, TypeID: id, Schema: schemas[id], Shape: shape})
		refOf[id] = name
	}
	return out, refOf
}

// errorBranchSuffix 错误分支原生写出的描述后缀。
const errorBranchSuffix = " (error branch)"

// rawRowCode 原生写出行的占位码（非负以免被当作「未解析」，且不进 code 枚举）。
const rawRowCode = 0

// defaultResponseText 未知 HTTP 状态码的响应描述。
const defaultResponseText = "Response"

// statusText HTTP 状态码的标准文本（如 201 → Created）。
func statusText(status int) string {
	if t := http.StatusText(status); t != "" {
		return t
	}
	return defaultResponseText
}

// llmEvidencePrefix LLM 事实证据引文的前缀（engine 写入，形如 "llm:error-code ErrX ← 源码行"）。
const llmEvidencePrefix = "llm:"

// llmEvidenceSep LLM 证据引文中「标签 ← 源码行」的分隔符。
const llmEvidenceSep = " ← "

// evidenceLabel 证据的展示标签：LLM 事实取其引文标签（如 "llm:error-code ErrX"），其余为路由注册点。
func evidenceLabel(ev facts.Evidence) string {
	if strings.HasPrefix(ev.Quote, llmEvidencePrefix) {
		label, _, _ := strings.Cut(ev.Quote, llmEvidenceSep)
		return label
	}
	return "route"
}

// componentName 把类型名规整为合法的 OpenAPI components 键（仅 [A-Za-z0-9._-]，其余替换为 _）。
func componentName(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			b[i] = '_'
		}
	}
	return string(b)
}

// canonicalShape schema 的规范化结构签名（去重键，不含描述性字段）。
func canonicalShape(sc *schema.Schema) string {
	var b strings.Builder
	writeShape(&b, sc, 0)
	return b.String()
}

func writeShape(b *strings.Builder, sc *schema.Schema, depth int) {
	if sc == nil || depth > 24 {
		b.WriteString("#")
		return
	}
	if sc.Ref != "" {
		b.WriteString("ref:" + sc.Ref)
		return
	}
	if len(sc.OneOf) > 0 {
		b.WriteString("one:")
		for _, variant := range sc.OneOf {
			writeShape(b, variant, depth+1)
		}
		return
	}
	fmt.Fprintf(b, "t:%s;f:%s;nb:%v;n:%v;ad:%v;", sc.Type, sc.Format, sc.NoBody, sc.Nullable, sc.Additional)
	if len(sc.Enum) > 0 {
		b.WriteString("e:" + strings.Join(sc.Enum, "|") + ";")
	}
	if sc.Min != nil {
		fmt.Fprintf(b, "min:%v;", *sc.Min)
	}
	if sc.Max != nil {
		fmt.Fprintf(b, "max:%v;", *sc.Max)
	}
	if sc.MinLen != nil {
		fmt.Fprintf(b, "ml:%v;", *sc.MinLen)
	}
	if sc.MaxLen != nil {
		fmt.Fprintf(b, "xl:%v;", *sc.MaxLen)
	}
	if sc.Items != nil {
		b.WriteString("i:")
		writeShape(b, sc.Items, depth+1)
	}
	for _, p := range sc.Props {
		fmt.Fprintf(b, "p:%s;req:%v;", p.Name, p.Required)
		writeShape(b, p.Schema, depth+1)
	}
}

func shortHash(s string) string {
	// FNV 快速哈希（P0 简化; P1 换 sha256 截断）
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return fmt.Sprintf("%06x", h&0xffffff)
}

// ---- 渲染入口（供 CLI 调用） --------------------------------------------

// CompileTwiceCheck 确定性双跑自检（设计文档 §8.5）:
// 对同一 Document 渲染两次，逐字节比对。
func CompileTwiceCheck(doc *Document) bool {
	y1, err1 := RenderYAML(doc)
	y2, err2 := RenderYAML(doc)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(y1) == string(y2)
}

// RenderYAML 输出最终 YAML 文本。
func RenderYAML(d *Document) ([]byte, error) {
	return render(d)
}

// ---- 辅助 --------------------------------------------------------------

func splitOpKey(opKey string) (method, path string) {
	// 兼容 "contract:op:METHOD:/path" 与 "op:METHOD:/path"
	s := strings.TrimPrefix(opKey, "contract:")
	s = strings.TrimPrefix(s, "op:")
	if i := strings.Index(s, ":"); i > 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}

func sortParams(ps []ParamOut) {
	sort.Slice(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		if inOrder(a.In) != inOrder(b.In) {
			return inOrder(a.In) < inOrder(b.In)
		}
		return a.Name < b.Name
	})
}

func inOrder(in string) int {
	switch in {
	case "path":
		return 0
	case "query":
		return 1
	case "header":
		return 2
	case "cookie":
		return 3
	}
	return 4
}

func methodOrder(m string) int {
	switch strings.ToLower(m) {
	case "get":
		return 0
	case "put":
		return 1
	case "post":
		return 2
	case "delete":
		return 3
	case "patch":
		return 4
	case "head":
		return 5
	case "options":
		return 6
	}
	return 9
}

// pathLess 路径分段字典序（设计文档 §8.5 规则 2）。
func pathLess(a, b string) bool {
	as := strings.Split(strings.TrimPrefix(a, "/"), "/")
	bs := strings.Split(strings.TrimPrefix(b, "/"), "/")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] != bs[i] {
			return as[i] < bs[i]
		}
	}
	return len(as) < len(bs)
}

func lastSeg(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

// pkgShort 类型 ID 的包名末段（sample-app/internal/trade.TsResp → trade），用于同名 schema 消歧。
func pkgShort(typeID string) string {
	seg := typeID
	if i := strings.LastIndex(seg, "/"); i >= 0 {
		seg = seg[i+1:]
	}
	if i := strings.Index(seg, "."); i >= 0 {
		seg = seg[:i]
	}
	return seg
}

func schemeTypeOf(sp *facts.SecurityPayload) string {
	switch sp.SchemeType {
	case "apiKey":
		return "apiKey"
	case "http":
		return "http"
	case "bearer":
		return "http"
	}
	return "apiKey"
}

// descSep 响应描述中各分支说明的分隔符。
const descSep = "; "

// appendDesc 追加一条分支说明；已存在的相同说明不重复（同码多变体的成功行只说明一次）。
func appendDesc(base, add string) string {
	if base == "" {
		return add
	}
	for _, part := range strings.Split(base, descSep) {
		if part == add {
			return base
		}
	}
	return base + descSep + add
}

func appendUniq(nums []int, v int) []int {
	for _, x := range nums {
		if x == v {
			return nums
		}
	}
	return append(nums, v)
}

func appendUniqStr(arr []string, v string) []string {
	for _, x := range arr {
		if x == v {
			return arr
		}
	}
	return append(arr, v)
}

// sitesOf 静态定性错误行的来源点摘要（只取表达式、去掉位置；最多 maxDescSites 个）。
func sitesOf(r facts.ResponseFact) string {
	var out []string
	for _, site := range facts.SplitSites(r.ErrSource) {
		if site = facts.SiteExpr(site); site != "" && !containsString(out, site) {
			out = append(out, site)
		}
	}
	if len(out) == 0 {
		return "source"
	}
	if len(out) > maxDescSites {
		out = append(out[:maxDescSites], "…")
	}
	return strings.Join(out, ", ")
}

// maxDescSites 响应描述中列出的来源表达式上限（完整来源见 operations.json / report）。
const maxDescSites = 3

// containsString 切片是否含 s。
func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
