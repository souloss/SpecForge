package golang

import (
	"context"
	"go/types"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/specforge/specforge/internal/frontend"
	"github.com/specforge/specforge/internal/infer"
	"github.com/specforge/specforge/internal/profile"
	"github.com/specforge/specforge/internal/slicing"
)

// L2 包装器摘要兜底：静态摘要失败、但「把形参写进响应」的函数，交给 LLM 生成一次摘要；
// 摘要经复核后注册为包装器模式，整个仓库所有调用点复用（一次调用修好一批 operation）。

// maxLLMWrappers 单次运行最多交给 LLM 摘要的包装器数。
const maxLLMWrappers = 20

// maxWrapperHelpers 包装器摘要任务里附带的辅助函数源码段数上限。
const maxWrapperHelpers = 5

// wrapperFieldTypes LLM 可报告的信封字段类型（封闭集合）。
var wrapperFieldTypes = map[string]bool{"string": true, "integer": true, "number": true, "boolean": true, "object": true, "array": true}

// 包装器摘要字段来源（infer.WrapperField.Source 取值）。
const (
	sourceData = "data"
	sourceErr  = "err"
)

// intLiteral 源码中的整数字面量（固定业务码的文本核对）。
var intLiteral = regexp.MustCompile(`-?\b[0-9]+\b`)

// summarizeWrappers 对未识别包装器逐个做 LLM 摘要并复核，返回通过复核的模式与合成信封及统计。
func summarizeWrappers(ctx context.Context, prog *Program, req frontend.Request, fnIDs []string,
	defaultStatus int) ([]profile.SinkPattern, map[string]slicing.SyntheticEnvelope, frontend.StepStats) {
	var st frontend.StepStats
	envs := map[string]slicing.SyntheticEnvelope{}
	var pats []profile.SinkPattern
	sort.Strings(fnIDs)
	if len(fnIDs) > maxLLMWrappers {
		fnIDs = fnIDs[:maxLLMWrappers]
	}
	for _, fn := range fnIDs {
		task, ok := wrapperTask(prog, fn)
		if !ok {
			continue
		}
		st.Attempted++
		sum, err := infer.SummarizeWrapper(ctx, req.Provider, task)
		if err != nil {
			st.Failed++
			req.Log.Warn("包装器摘要失败，保持缺口", "wrapper", fn, "err", err)
			continue
		}
		pat, env, ok := verifyWrapperSummary(prog, fn, task, sum, defaultStatus)
		if !ok {
			st.Rejected++
			req.Log.Warn("包装器摘要未通过复核，丢弃", "wrapper", fn)
			continue
		}
		st.Accepted++
		pats = append(pats, pat)
		envs[env.ID] = env
		req.Log.Info("包装器摘要已采纳", "wrapper", fn, "data_param", sum.DataParam, "err_param", sum.ErrParam)
	}
	return pats, envs, st
}

// wrapperTask 组装摘要任务：形参 + 函数源码 + 其直接调用的仓库内辅助函数源码。
func wrapperTask(prog *Program, fn string) (infer.WrapperTask, bool) {
	g := prog.Graph()
	fd, info := g.FuncDeclOf(fn), g.TypeInfoOfFunc(fn)
	file, start, _, text, ok := prog.FuncSource(fn)
	if fd == nil || info == nil || !ok || fd.Type.Params == nil {
		return infer.WrapperTask{}, false
	}
	task := infer.WrapperTask{Symbol: fn, Sources: []infer.SourceSnippet{{Symbol: fn, File: prog.RelPath(file), Line: start, Code: text}}}
	idx := 0
	for _, field := range fd.Type.Params.List {
		t := info.TypeOf(field.Type)
		names := field.Names
		if len(names) == 0 {
			task.Params = append(task.Params, infer.WrapperParam{Index: idx, Type: typeString(t)})
			idx++
			continue
		}
		for _, n := range names {
			task.Params = append(task.Params, infer.WrapperParam{Index: idx, Name: n.Name, Type: typeString(t)})
			idx++
		}
	}
	for _, callee := range prog.Callees(fn) {
		if len(task.Sources) > maxWrapperHelpers {
			break
		}
		if f, s, _, t, ok := prog.FuncSource(callee); ok {
			task.Sources = append(task.Sources, infer.SourceSnippet{Symbol: callee, File: prog.RelPath(f), Line: s, Code: t})
		}
	}
	return task, true
}

// typeString 类型串（包名简写）。
func typeString(t types.Type) string {
	if t == nil {
		return ""
	}
	return types.TypeString(t, func(p *types.Package) string { return p.Name() })
}

// verifyWrapperSummary 复核 LLM 摘要：形参下标合法且类型相容（err 形参须实现 error），每个信封键都以
// 字符串字面量出现在源码中，类型取封闭集合，固定业务码以整数字面量出现；通过则产出包装器模式与合成信封。
func verifyWrapperSummary(prog *Program, fn string, task infer.WrapperTask, sum *infer.WrapperSummary,
	defaultStatus int) (profile.SinkPattern, slicing.SyntheticEnvelope, bool) {
	var src strings.Builder
	for _, s := range task.Sources {
		src.WriteString(s.Code)
		src.WriteByte('\n')
	}
	text := src.String()
	n := len(task.Params)
	if sum.DataParam < -1 || sum.DataParam >= n || sum.ErrParam < -1 || sum.ErrParam >= n ||
		(sum.DataParam >= 0 && sum.DataParam == sum.ErrParam) || (sum.DataParam < 0 && sum.ErrParam < 0) {
		return profile.SinkPattern{}, slicing.SyntheticEnvelope{}, false
	}
	if sum.ErrParam >= 0 && !paramIsError(prog, fn, sum.ErrParam) {
		return profile.SinkPattern{}, slicing.SyntheticEnvelope{}, false
	}
	for _, code := range []*int{sum.SuccessCode, sum.FailureCode} {
		if code != nil && !containsInt(text, *code) {
			return profile.SinkPattern{}, slicing.SyntheticEnvelope{}, false
		}
	}
	name := fn[strings.LastIndex(fn, ".")+1:]
	env := slicing.SyntheticEnvelope{ID: slicing.SyntheticEnvPrefix + fn + "." + name + "Envelope", Fn: fn}
	pat := profile.SinkPattern{
		Symbol: fn, DataSlot: sum.DataParam, ErrSlot: sum.ErrParam, Status: defaultStatus, Auto: true, Learned: true,
		EnvelopeType: env.ID, ErrEnvelopeType: env.ID, SuccessCode: sum.SuccessCode, FailureCode: sum.FailureCode,
		BranchOnErr: sum.DataParam >= 0 && sum.ErrParam >= 0,
	}
	if pat.DataSlot < 0 {
		pat.DataSlot = profile.NoSlot
	}
	if pat.ErrSlot < 0 {
		pat.ErrSlot = profile.NoSlot
	}
	for _, f := range sum.Fields {
		if f.Key == "" || !strings.Contains(text, strconv.Quote(f.Key)) || !wrapperFieldTypes[f.Type] {
			return profile.SinkPattern{}, slicing.SyntheticEnvelope{}, false // 源码中没有该键字面量 / 类型集外
		}
		fld := slicing.EnvelopeField{Key: f.Key, JSONType: f.Type}
		switch f.Source {
		case sourceData:
			fld.Role, pat.DataField = slicing.RoleData, f.Key
		case sourceErr:
			fld.Role, pat.ErrField = slicing.RoleErr, f.Key
		}
		env.Fields = append(env.Fields, fld)
	}
	if sum.DataParam >= 0 && pat.DataField == "" {
		return profile.SinkPattern{}, slicing.SyntheticEnvelope{}, false // 声称有 data 形参却没有承载它的键
	}
	return pat, env, true
}

// paramIsError 包装器第 idx 个形参的类型是否实现 error。
func paramIsError(prog *Program, fn string, idx int) bool {
	g := prog.Graph()
	fd, info := g.FuncDeclOf(fn), g.TypeInfoOfFunc(fn)
	i := 0
	for _, field := range fd.Type.Params.List {
		count := len(field.Names)
		if count == 0 {
			count = 1
		}
		if idx < i+count {
			t := info.TypeOf(field.Type)
			return t != nil && types.Implements(t, errorType)
		}
		i += count
	}
	return false
}

// errorType 内置 error 接口。
var errorType = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)

// containsInt 源码中是否以整数字面量出现 v。
func containsInt(text string, v int) bool {
	want := strconv.Itoa(v)
	for _, m := range intLiteral.FindAllString(text, -1) {
		if m == want {
			return true
		}
	}
	return false
}
