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

	"gopkg.in/yaml.v3"
)

// Metrics 评测结果。
type Metrics struct {
	ParamMisses    []string
	ParamSpurious  []string
	RouteRecall    float64
	RoutePrecision float64
	ParamF1        float64
	ParamPrec      float64
	ParamRec       float64
	ReqFieldF1     float64
	ReqFieldPrec   float64
	ReqFieldRec    float64
	RespFieldF1    float64
	RespFieldPrec  float64
	RespFieldRec   float64
	EnvelopeRecall float64
	HallucRate     float64
	MissingRoutes  []string
	ExtraRoutes    []string
	FieldMisses    []string
	FieldSpurious  []string
	EnvelopMisses  []string
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
	Ref      string               `yaml:"$ref"`
	Type     interface{}          `yaml:"type"`
	Format   string               `yaml:"format"`
	Enum     []interface{}        `yaml:"enum"`
	Props    map[string]oasSchema `yaml:"properties"`
	Required []string             `yaml:"required"`
	Items    *oasSchema           `yaml:"items"`
	Nullable bool                 `yaml:"nullable"`
}

func loadDoc(path string) (*oasDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d oasDoc
	if err := yaml.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &d, nil
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
			reqTP, reqFP, reqFN = accumulate(reqTP, reqFP, reqFN, sf, tf)
			totalFields += len(tf)

			// 响应字段（200 信封 data 槽展平）
			sr := flattenRespData(specOp.Responses, spec.Components.Schemas)
			tr := flattenRespData(truthOp.Responses, truth.Components.Schemas)
			respTP, respFP, respFN = accumulate(respTP, respFP, respFN, sr, tr)
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

// flattenResolve $ref → components 解析后再展平（嵌套任意层）。
func flattenResolve(s *oasSchema, prefix string, withRequired bool, comps map[string]oasSchema, depth int) []string {
	if s == nil || depth > 8 {
		return nil
	}
	if s.Ref != "" {
		name := refName(s.Ref)
		if sc, ok := comps[name]; ok {
			local := sc
			return flattenSchema(&local, prefix, withRequired, comps, depth+1)
		}
		return []string{prefix + ":$ref:" + name}
	}
	return flattenSchema(s, prefix, withRequired, comps, depth)
}

// flattenRespData 200 响应信封的 data 槽展平。
func flattenRespData(rs map[string]oasResp, comps map[string]oasSchema) []string {
	r, ok := rs["200"]
	if !ok {
		return nil
	}
	for _, mt := range r.Content {
		data, ok := mt.Schema.Props["data"]
		if !ok {
			return nil
		}
		if data.Ref != "" || (data.Type != nil && data.Items != nil) {
			return flattenResolve(&data, "data", true, comps, 0)
		}
		return flattenSchema(&data, "data", true, comps, 0)
	}
	return nil
}

// flattenSchema 递归展平（设计文档 §11.1: 嵌套展平后比较; 嵌套 $ref 经 comps 解析）。
func flattenSchema(s *oasSchema, prefix string, withRequired bool, comps map[string]oasSchema, depth int) []string {
	if s == nil || depth > 8 {
		return nil
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
		t := typeStr(p)
		out = append(out, fmt.Sprintf("%s|%s|%v", path, t, reqSet[k] || !withRequired))
		if p.Props != nil {
			out = append(out, flattenResolve(p, path, withRequired, comps, depth+1)...)
		}
		if p.Items != nil {
			out = append(out, flattenResolve(p.Items, path+"[]", withRequired, comps, depth+1)...)
		}
	}
	return out
}

func typeStr(s *oasSchema) string {
	if s.Ref != "" {
		return "$ref"
	}
	switch t := s.Type.(type) {
	case string:
		if s.Format != "" {
			return t + "/" + s.Format
		}
		return t
	case []interface{}:
		// 3.1 type 数组（可空）
		var parts []string
		for _, v := range t {
			if s, ok := v.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "|")
	}
	return "?"
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

func splitKey(k string) (method, path string) {
	i := strings.Index(k, " ")
	if i > 0 {
		return strings.ToUpper(k[:i]), k[i+1:]
	}
	return "", k
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
Response Field F1 : %.3f
Envelope Recall   : %.3f
Hallucination     : %.3f
`, m.RouteRecall, len(m.MissingRoutes), m.RoutePrecision, len(m.ExtraRoutes),
		m.ParamF1, m.ReqFieldF1, m.RespFieldF1, m.EnvelopeRecall, m.HallucRate)

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

// WriteJSON 机器可读输出。
func WriteJSON(m *Metrics, w io.Writer) error {
	fmt.Fprintf(w, `{"route_recall":%.4f,"route_precision":%.4f,"param_f1":%.4f,"req_field_f1":%.4f,"resp_field_f1":%.4f,"envelope_recall":%.4f,"hallucination":%.4f}`+"\n",
		m.RouteRecall, m.RoutePrecision, m.ParamF1, m.ReqFieldF1, m.RespFieldF1, m.EnvelopeRecall, m.HallucRate)
	return nil
}
