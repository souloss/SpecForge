// Package eval 评测基准（设计文档 §11.1 指标精确定义）。
//
//	Route Recall/Precision: (method, path) 集合匹配
//	Param F1:               (in, name, required, type) 四元组 micro
//	Request Field F1:       (json path, type, required) 展平匹配
//	Response Field F1:      同上（信封 data 槽 schema）
//	Envelope Code Recall:   (status, envelope.code) 二元组集合
//	Hallucination:          生成 spec 有而 truth 无且无证据的字段占比
package eval

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// Metrics 评测结果。
type Metrics struct {
	ParamMisses    []string `json:"param_misses"`    // 参数漏报（in:name）
	ParamSpurious  []string `json:"param_spurious"`  // 参数误报
	RouteRecall    float64  `json:"route_recall"`    // 路由召回
	RoutePrecision float64  `json:"route_precision"` // 路由精确率
	ParamF1        float64  `json:"param_f1"`        // 参数四元组 F1
	ParamPrec      float64  `json:"param_precision"` // 参数精确率
	ParamRec       float64  `json:"param_recall"`    // 参数召回
	ReqFieldF1     float64  `json:"req_field_f1"`    // 请求体字段 F1
	ReqFieldPrec   float64  `json:"req_field_precision"`
	ReqFieldRec    float64  `json:"req_field_recall"`
	RespFieldF1    float64  `json:"resp_field_f1"` // 响应字段 F1（含 required）
	RespFieldPrec  float64  `json:"resp_field_precision"`
	RespFieldRec   float64  `json:"resp_field_recall"`
	RespShapeF1    float64  `json:"resp_shape_f1"`   // 响应字段 F1（仅 path+type，不比 required：swag 仅凭校验 tag 标 required，与序列化语义口径不同）
	EnvelopeRecall float64  `json:"envelope_recall"` // 信封码/状态码召回
	HallucRate     float64  `json:"hallucination"`   // 幻觉率：spec 中无真值对应的字段占比
	MissingRoutes  []string `json:"missing_routes"`  // 漏报路由
	ExtraRoutes    []string `json:"extra_routes"`    // 多报路由
	FieldMisses    []string `json:"field_misses"`    // 响应字段漏报（label: path|type|required）
	FieldSpurious  []string `json:"field_spurious"`  // 响应字段误报
	ReqMisses      []string `json:"req_misses"`      // 请求体字段漏报
	ReqSpurious    []string `json:"req_spurious"`    // 请求体字段误报
	EnvelopMisses  []string `json:"envelope_misses"` // 信封码漏报
}

// ---- 解析（truth 与 spec 共用同一 OpenAPI 结构） ------------------------

type oasDoc struct {
	Paths      map[string]map[string]oasOp `yaml:"paths"`
	Components oasComponents               `yaml:"components"`
}

type oasComponents struct {
	Schemas map[string]oasSchema `yaml:"schemas"`
}

type oasOp struct {
	OperationID string             `yaml:"operationId"`
	Parameters  []oasParam         `yaml:"parameters"`
	RequestBody *oasBody           `yaml:"requestBody"`
	Responses   map[string]oasResp `yaml:"responses"`
}

type oasParam struct {
	Name     string    `yaml:"name"`
	In       string    `yaml:"in"`
	Required bool      `yaml:"required"`
	Schema   oasSchema `yaml:"schema"`
}

type oasBody struct {
	Content map[string]struct {
		Schema oasSchema `yaml:"schema"`
	} `yaml:"content"`
}

type oasResp struct {
	Description string `yaml:"description"`
	Content     map[string]struct {
		Schema oasSchema `yaml:"schema"`
	} `yaml:"content"`
}

type oasSchema struct {
	Ref                  string               `yaml:"$ref"`
	Type                 interface{}          `yaml:"type"`
	Format               string               `yaml:"format"`
	Enum                 []interface{}        `yaml:"enum"`
	Props                map[string]oasSchema `yaml:"properties"`
	Required             []string             `yaml:"required"`
	Items                *oasSchema           `yaml:"items"`
	Nullable             bool                 `yaml:"nullable"`
	AdditionalProperties interface{}          `yaml:"additionalProperties"`
	AllOf                []oasSchema          `yaml:"allOf"` // 组合 schema（swag 信封收窄 `Envelope{result=T}` 的产出形态）
}

// maxFlattenDepth schema 展平的递归深度上限（防自引用类型无限展开）。
const maxFlattenDepth = 8

func loadDoc(path string) (*oasDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	d, err := parseDoc(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return d, nil
}

// Evaluate 对比 truth 与生成 spec。
func Evaluate(truthPath, specPath string) (*Metrics, error) {
	truth, err := loadDoc(truthPath)
	if err != nil {
		return nil, err
	}
	spec, err := loadDoc(specPath)
	if err != nil {
		return nil, err
	}
	m := &Metrics{}

	// ---- 路由集合 ----
	truthRoutes := routeSet(truth)
	specRoutes := routeSet(spec)
	m.MissingRoutes = setDiff(truthRoutes, specRoutes)
	m.ExtraRoutes = setDiff(specRoutes, truthRoutes)
	m.RouteRecall = recall(truthRoutes, specRoutes)
	m.RoutePrecision = precision(specRoutes, truthRoutes)

	// ---- 逐 operation 参数 / 字段 / 信封 ----
	var paramTP, paramFP, paramFN int
	var reqTP, reqFP, reqFN int
	var respTP, respFP, respFN int
	var shapeTP, shapeFP, shapeFN int
	var envTP, envFN int
	var spuriousFields int
	var totalFields int

	for path, methods := range spec.Paths {
		tops, ok := truth.Paths[path]
		if !ok {
			continue
		}
		for method, specOp := range methods {
			truthOp, ok := tops[method]
			if !ok {
				continue
			}
			label := strings.ToUpper(method) + " " + path

			// 参数四元组
			sp := paramQuads(specOp)
			tp := paramQuads(truthOp)
			tpCount, fpCount, fnCount := setCounts(sp, tp)
			paramTP += tpCount
			paramFP += fpCount
			paramFN += fnCount
			for _, q := range setDiffStr(tp, sp) {
				m.ParamMisses = append(m.ParamMisses, label+": "+q)
			}
			for _, q := range setDiffStr(sp, tp) {
				m.ParamSpurious = append(m.ParamSpurious, label+": "+q)
			}

			// 请求字段（展平）
			sf := flattenBody(specOp.RequestBody, spec.Components.Schemas)
			tf := flattenBody(truthOp.RequestBody, truth.Components.Schemas)
			sf = allowOpenObjectExtras(sf, tf, openBodyPaths(truthOp.RequestBody, truth.Components.Schemas))
			reqTP, reqFP, reqFN = accumulate(reqTP, reqFP, reqFN, sf, tf)
			totalFields += len(tf)
			for _, f := range setDiffStr(tf, sf) {
				m.ReqMisses = append(m.ReqMisses, label+": "+f)
			}
			for _, f := range setDiffStr(sf, tf) {
				m.ReqSpurious = append(m.ReqSpurious, label+": "+f)
			}

			// 响应字段（200 响应体整体展平，含信封字段）
			sr := flattenRespData(specOp.Responses, spec.Components.Schemas)
			tr := flattenRespData(truthOp.Responses, truth.Components.Schemas)
			sr = allowOpenObjectExtras(sr, tr, openRespDataPaths(truthOp.Responses, truth.Components.Schemas))
			respTP, respFP, respFN = accumulate(respTP, respFP, respFN, sr, tr)
			shapeTP, shapeFP, shapeFN = accumulate(shapeTP, shapeFP, shapeFN, dropRequired(sr), dropRequired(tr))
			totalFields += len(tr)
			for _, f := range setDiffStr(sr, tr) {
				m.FieldSpurious = append(m.FieldSpurious, label+": "+f)
				spuriousFields++
			}
			for _, f := range setDiffStr(tr, sr) {
				m.FieldMisses = append(m.FieldMisses, label+": "+f)
			}

			// 信封码
			se := envelopeCodes(specOp.Responses)
			te := envelopeCodes(truthOp.Responses)
			envTP += countIntersect(se, te)
			envFN += len(setDiffStr(te, se))
			for _, e := range setDiffStr(te, se) {
				m.EnvelopMisses = append(m.EnvelopMisses, label+": "+e)
			}
		} // 内层 method 循环
	} // 外层 path 循环

	m.ParamPrec, m.ParamRec, m.ParamF1 = prf1(paramTP, paramFP, paramFN)
	m.ReqFieldPrec, m.ReqFieldRec, m.ReqFieldF1 = prf1(reqTP, reqFP, reqFN)
	m.RespFieldPrec, m.RespFieldRec, m.RespFieldF1 = prf1(respTP, respFP, respFN)
	_, _, m.RespShapeF1 = prf1(shapeTP, shapeFP, shapeFN)
	envTotal := envTP + envFN
	if envTotal > 0 {
		m.EnvelopeRecall = float64(envTP) / float64(envTotal)
	}
	if totalFields > 0 {
		m.HallucRate = float64(spuriousFields) / float64(totalFields)
	}
	return m, nil
}

// ---- 字段展平 ----------------------------------------------------------

// flattenBody 请求体 schema 展平为 (path, type, required) 三元组串。
func flattenBody(b *oasBody, comps map[string]oasSchema) []string {
	if b == nil {
		return nil
	}
	for _, mt := range b.Content {
		return flattenResolve(&mt.Schema, "", true, comps, 0)
	}
	return nil
}

// openBodyPaths returns object paths whose truth schema permits arbitrary
// additional properties. Concrete keys under those objects are compatible
// with the contract and must not count as spurious fields.
func openBodyPaths(b *oasBody, comps map[string]oasSchema) map[string]bool {
	if b == nil {
		return nil
	}
	for _, mt := range b.Content {
		return openSchemaPaths(&mt.Schema, "", comps, 0)
	}
	return nil
}

// openRespDataPaths is the response counterpart of openBodyPaths. Evaluation
// compares the successful response body, so only its 200 schema is relevant.
func openRespDataPaths(rs map[string]oasResp, comps map[string]oasSchema) map[string]bool {
	r, ok := rs[statusOK]
	if !ok {
		return nil
	}
	for _, mt := range r.Content {
		return openSchemaPaths(&mt.Schema, "", comps, 0)
	}
	return nil
}

// openSchemaPaths walks a schema while resolving refs and allOf branches.
func openSchemaPaths(s *oasSchema, prefix string, comps map[string]oasSchema, depth int) map[string]bool {
	paths := map[string]bool{}
	if s == nil || depth > maxFlattenDepth {
		return paths
	}
	if s.Ref != "" {
		name := refName(s.Ref)
		if sc, ok := comps[name]; ok {
			return openSchemaPaths(&sc, prefix, comps, depth)
		}
		return paths
	}
	if len(s.AllOf) > 0 {
		merged := mergeAllOf(s, comps, depth)
		s = &merged
	}
	if additionalPropertiesOpen(s.AdditionalProperties) {
		paths[prefix] = true
	}
	for name, prop := range s.Props {
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		for p := range openSchemaPaths(&prop, path, comps, depth+1) {
			paths[p] = true
		}
	}
	if s.Items != nil {
		path := prefix + "[]"
		for p := range openSchemaPaths(s.Items, path, comps, depth+1) {
			paths[p] = true
		}
	}
	return paths
}

func additionalPropertiesOpen(v interface{}) bool {
	switch value := v.(type) {
	case bool:
		return value
	case map[string]interface{}:
		return true
	case map[interface{}]interface{}:
		return true
	default:
		return false
	}
}

// allowOpenObjectExtras removes generated descendants that are permitted by
// an open object in truth. Explicitly declared truth fields are retained so
// their types and requiredness continue to be compared.
func allowOpenObjectExtras(spec, truth []string, open map[string]bool) []string {
	if len(spec) == 0 || len(open) == 0 {
		return spec
	}
	truthPaths := make(map[string]bool, len(truth))
	for _, field := range truth {
		path := field
		if i := strings.IndexByte(path, '|'); i >= 0 {
			path = path[:i]
		}
		truthPaths[path] = true
	}
	out := make([]string, 0, len(spec))
	for _, field := range spec {
		path := field
		if i := strings.IndexByte(path, '|'); i >= 0 {
			path = path[:i]
		}
		if truthPaths[path] || !underOpenObject(path, open) {
			out = append(out, field)
		}
	}
	return out
}

func underOpenObject(path string, open map[string]bool) bool {
	for parent := range open {
		if parent == "" || strings.HasPrefix(path, parent+".") || strings.HasPrefix(path, parent+"[]") {
			return true
		}
	}
	return false
}

// flattenResolve $ref → components 解析后再展平（嵌套任意层）。
func flattenResolve(s *oasSchema, prefix string, withRequired bool, comps map[string]oasSchema, depth int) []string {
	if s == nil || depth > maxFlattenDepth {
		return nil
	}
	if s.Ref != "" {
		name := refName(s.Ref)
		if sc, ok := comps[name]; ok {
			// 深度只按嵌套层级计：$ref 与内联是同一结构的两种表达，不应让引用多截断一层
			local := sc
			return flattenSchema(&local, prefix, withRequired, comps, depth)
		}
		return []string{prefix + ":$ref:" + name}
	}
	return flattenSchema(s, prefix, withRequired, comps, depth)
}

// flattenRespData 200 响应体整体展平（信封字段 + 业务体字段）。
//
// 不假设信封形态（code/msg/data、jsonrpc/result/error 等均可）：信封本身就是
// 契约的一部分，按真实响应体结构比较才能暴露「信封形态写错」这类缺陷。
func flattenRespData(rs map[string]oasResp, comps map[string]oasSchema) []string {
	r, ok := rs[statusOK]
	if !ok {
		return nil
	}
	for _, mt := range r.Content {
		body := mt.Schema
		return flattenResolve(&body, "", true, comps, 0)
	}
	return nil
}

// statusOK 成功响应的状态码键。
const statusOK = "200"

// flattenSchema 递归展平（设计文档 §11.1: 嵌套展平后比较; 嵌套 $ref 经 comps 解析）。
func flattenSchema(s *oasSchema, prefix string, withRequired bool, comps map[string]oasSchema, depth int) []string {
	if s == nil || depth > maxFlattenDepth {
		return nil
	}
	if len(s.AllOf) > 0 {
		merged := mergeAllOf(s, comps, depth)
		s = &merged
	}
	var out []string
	// required 集合
	reqSet := map[string]bool{}
	for _, r := range s.Required {
		reqSet[r] = true
	}
	keys := make([]string, 0, len(s.Props))
	for k := range s.Props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		pv := s.Props[k]
		p := &pv
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		t := resolvedTypeStr(p, comps)
		out = append(out, fmt.Sprintf("%s|%s|%v", path, t, reqSet[k] || !withRequired))
		if p.Props != nil || p.Ref != "" || len(p.AllOf) > 0 {
			out = append(out, flattenResolve(p, path, withRequired, comps, depth+1)...)
		}
		if p.Items != nil {
			out = append(out, flattenResolve(p.Items, path+"[]", withRequired, comps, depth+1)...)
		}
	}
	return out
}

// resolvedTypeStr 字段类型串；$ref 解析到目标 schema 的类型再比较——
// 命名类型以 $ref 引用还是内联展开只是表达差异，不应判为类型不一致。
func resolvedTypeStr(s *oasSchema, comps map[string]oasSchema) string {
	for depth := 0; s.Ref != "" && depth <= maxFlattenDepth; depth++ {
		target, ok := comps[refName(s.Ref)]
		if !ok {
			return typeStr(s)
		}
		s = &target
	}
	if s.Type == nil && (len(s.Props) > 0 || len(s.AllOf) > 0) {
		return "object"
	}
	return typeStr(s)
}

// nullType OAS 3.1 type 数组中表示可空的类型名。
const nullType = "null"

func typeStr(s *oasSchema) string {
	if s.Ref != "" {
		return "$ref"
	}
	if len(s.AllOf) > 0 {
		return "object"
	}
	switch t := s.Type.(type) {
	case string:
		// 数值位宽格式（int32/int64/float/double）是实现细节，swag 等工具不输出，不参与比较。
		if s.Format != "" && t != "integer" && t != "number" {
			return t + "/" + s.Format
		}
		return t
	case []interface{}:
		// 3.1 type 数组（可空）：可空性不参与比较——swagger 2.0 真值无法表达 null，
		// 否则同一字段会因版本表达差异被判为不一致
		var parts []string
		for _, v := range t {
			if s, ok := v.(string); ok && s != nullType {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "|")
	}
	return "?"
}

// dropRequired 去掉展平三元组的 required 分量（path|type|required → path|type）。
func dropRequired(fields []string) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if i := strings.LastIndex(f, "|"); i >= 0 {
			f = f[:i]
		}
		out = append(out, f)
	}
	return out
}

// ---- 参数四元组 --------------------------------------------------------

func paramQuads(op oasOp) []string {
	var out []string
	for _, p := range op.Parameters {
		t := typeStr(&p.Schema)
		out = append(out, fmt.Sprintf("%s|%s|%v|%s", p.In, p.Name, p.Required, t))
	}
	sort.Strings(out)
	return out
}

func envelopeCodes(rs map[string]oasResp) []string {
	var out []string
	for status, r := range rs {
		// 首选: 信封 schema properties.code 的 enum（主通道）
		found := false
		for _, mt := range r.Content {
			code, ok := mt.Schema.Props["code"]
			if !ok {
				continue
			}
			for _, v := range code.Enum {
				out = append(out, fmt.Sprintf("%s:%v", status, v))
			}
			if len(code.Enum) > 0 {
				found = true
			}
		}
		if found {
			continue
		}
		// 兜底: description 中的 code=N（全部出现）
		for _, seg := range allAfter(r.Description, "code=") {
			out = append(out, status+":"+seg)
		}
		if status == "200" && !strings.Contains(r.Description, "code") {
			out = append(out, status+":0")
		}
	}
	sort.Strings(out)
	return out
}

func allAfter(desc, marker string) []string {
	var out []string
	for {
		i := strings.Index(desc, marker)
		if i < 0 {
			break
		}
		desc = desc[i+len(marker):]
		seg := desc
		if j := strings.IndexAny(seg, " :);,"); j >= 0 {
			seg = seg[:j]
		}
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

func refName(ref string) string { return lastSeg(ref) }

// ---- 集合辅助 ----------------------------------------------------------

func routeSet(d *oasDoc) map[string]bool {
	out := map[string]bool{}
	for path, methods := range d.Paths {
		for m := range methods {
			out[strings.ToUpper(m)+" "+path] = true
		}
	}
	return out
}

func setDiff(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func setDiffStr(a, b []string) []string {
	bs := map[string]bool{}
	for _, x := range b {
		bs[x] = true
	}
	var out []string
	for _, x := range a {
		if !bs[x] {
			out = append(out, x)
		}
	}
	return out
}

func recall(truth, spec map[string]bool) float64 {
	if len(truth) == 0 {
		return 1
	}
	hit := 0
	for k := range truth {
		if spec[k] {
			hit++
		}
	}
	return float64(hit) / float64(len(truth))
}

func precision(spec, truth map[string]bool) float64 {
	if len(spec) == 0 {
		return 0
	}
	hit := 0
	for k := range spec {
		if truth[k] {
			hit++
		}
	}
	return float64(hit) / float64(len(spec))
}

func setCounts(spec, truth []string) (tp, fp, fn int) {
	ts := map[string]bool{}
	for _, t := range truth {
		ts[t] = true
	}
	ss := map[string]bool{}
	for _, s := range spec {
		ss[s] = true
	}
	for s := range ss {
		if ts[s] {
			tp++
		} else {
			fp++
		}
	}
	for t := range ts {
		if !ss[t] {
			fn++
		}
	}
	return
}

func accumulate(tp, fp, fn int, spec, truth []string) (int, int, int) {
	a, b, c := setCounts(spec, truth)
	return tp + a, fp + b, fn + c
}

func countIntersect(a, b []string) int {
	bs := map[string]bool{}
	for _, x := range b {
		bs[x] = true
	}
	n := 0
	for _, x := range a {
		if bs[x] {
			n++
		}
	}
	return n
}

func prf1(tp, fp, fn int) (p, r, f float64) {
	if tp+fp > 0 {
		p = float64(tp) / float64(tp+fp)
	}
	if tp+fn > 0 {
		r = float64(tp) / float64(tp+fn)
	}
	if p+r > 0 {
		f = 2 * p * r / (p + r)
	}
	return
}

func lastSeg(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// ---- 输出 --------------------------------------------------------------

// WriteReport 人类可读报告。
func WriteReport(m *Metrics, w io.Writer) error {
	fmt.Fprintf(w, `SpecForge Evaluation
================================
Route Recall      : %.3f   (missing %d)
Route Precision   : %.3f   (extra %d)
Param F1          : %.3f
Request Field F1  : %.3f
Response Field F1 : %.3f   (shape-only, ignoring required: %.3f)
Envelope Recall   : %.3f
Hallucination     : %.3f
`, m.RouteRecall, len(m.MissingRoutes), m.RoutePrecision, len(m.ExtraRoutes),
		m.ParamF1, m.ReqFieldF1, m.RespFieldF1, m.RespShapeF1, m.EnvelopeRecall, m.HallucRate)

	if len(m.MissingRoutes) > 0 {
		fmt.Fprintf(w, "\nMissing routes:\n")
		for _, r := range m.MissingRoutes {
			fmt.Fprintf(w, "  - %s\n", r)
		}
	}
	if len(m.ExtraRoutes) > 0 {
		fmt.Fprintf(w, "\nExtra (spurious) routes:\n")
		for _, r := range m.ExtraRoutes {
			fmt.Fprintf(w, "  - %s\n", r)
		}
	}
	if len(m.ParamMisses) > 0 {
		fmt.Fprintf(w, "\nMissing params (%d):\n", len(m.ParamMisses))
		for _, q := range m.ParamMisses {
			fmt.Fprintf(w, "  - %s\n", q)
		}
	}
	if len(m.ParamSpurious) > 0 {
		fmt.Fprintf(w, "\nSpurious params (%d):\n", len(m.ParamSpurious))
		for _, q := range m.ParamSpurious {
			fmt.Fprintf(w, "  - %s\n", q)
		}
	}
	printList(w, "Missing request fields", m.ReqMisses)
	printList(w, "Spurious request fields", m.ReqSpurious)
	if len(m.FieldMisses) > 0 {
		fmt.Fprintf(w, "\nMissing fields (%d):\n", len(m.FieldMisses))
		for _, f := range m.FieldMisses {
			fmt.Fprintf(w, "  - %s\n", f)
		}
	}
	if len(m.FieldSpurious) > 0 {
		fmt.Fprintf(w, "\nSpurious fields (%d):\n", len(m.FieldSpurious))
		for _, f := range m.FieldSpurious {
			fmt.Fprintf(w, "  - %s\n", f)
		}
	}
	if len(m.EnvelopMisses) > 0 {
		fmt.Fprintf(w, "\nMissing envelope codes (%d):\n", len(m.EnvelopMisses))
		for _, e := range m.EnvelopMisses {
			fmt.Fprintf(w, "  - %s\n", e)
		}
	}
	return nil
}

// printList 输出一个带计数标题的明细列表（空列表不输出）。
func printList(w io.Writer, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s (%d):\n", title, len(items))
	for _, it := range items {
		fmt.Fprintf(w, "  - %s\n", it)
	}
}
