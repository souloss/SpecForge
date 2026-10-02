package golang

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/specforge/specforge/internal/adapter"
	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/profile"
	"github.com/specforge/specforge/internal/schema"
	"github.com/specforge/specforge/internal/slicing"
	"github.com/specforge/specforge/internal/typeschema"
)

// ContractPayloadBuilder 单 operation 的契约构造（档位 1 纯静态路径）。
type ContractPayloadBuilder struct {
	g           *codegraph.Graph
	prof        *profile.Profile
	synth       *typeschema.Synthesizer
	reqSynth    *typeschema.Synthesizer // 请求体方向合成器（必填只认校验约束）
	route       adapter.Route
	slicer      *slicing.Slicer
	catalog     map[string]slicing.ErrorCodeEntry
	binders     map[string]slicing.Binder            // 请求绑定包装器摘要（符号 ID → 摘要）
	envs        map[string]slicing.SyntheticEnvelope // map 字面量信封的合成定义（合成类型 ID → 定义）
	wrapperGaps []string                             // 本 operation 中未被识别的响应包装器（函数符号 ID）
	learnedEv   []facts.Evidence                     // 来自 LLM 包装器摘要的证据（包装器声明位置）
	prims       *primIndex                           // 框架原语索引（请求绑定/参数读取）
	opKey       string
	evFile      string
	blob        string
}

// Build 产出 contract 事实 + 关联 schema/security 事实。
func (b *ContractPayloadBuilder) Build() (*facts.Fact, []*facts.Fact, []*facts.Fact) {
	cp := facts.ContractPayload{OperationID: operationIDOf(b.route)}
	conf := 1.0
	var unknowns []string

	// ---- 中间件 → security / header 参数（F7，profile 驱动） ----
	var securityNames []string
	var extra []*facts.Fact
	seenHeaders := map[string]bool{}
	for _, mw := range b.route.Middleware {
		mapping, ok := b.prof.AuthMiddleware[middlewareKey(mw)]
		if !ok {
			continue
		}
		if mapping.Header != "" && !seenHeaders[mapping.Header] {
			seenHeaders[mapping.Header] = true
			cp.Params = append(cp.Params, facts.ParamFact{
				In: "header", Name: mapping.Header, Required: mapping.Required,
				Type:   "string",
				Origin: "middleware:" + lastSeg(mw),
			})
		}
		if mapping.Scheme != "" {
			name := schemeNameOf(mapping)
			if !containsStr(securityNames, name) {
				securityNames = append(securityNames, name)
			}
			if b.routedScheme(name) == nil {
				extra = append(extra, &facts.Fact{
					ID: "security:" + name, Kind: facts.KindSecurity,
					Value: facts.SecurityPayload{
						MiddlewareSymbol: mw, SchemeType: mapping.Scheme,
						Header: mapping.Header, Required: mapping.Required,
						ScopeArg: scopeArgOf(b.route, mw),
					},
					Source: facts.SourceStatic, Confidence: 0.85,
					Evidence: []facts.Evidence{b.routeEvidence(mw)},
					Status:   "verified",
				})
			}
		}
	}
	cp.Security = securityNames
	cp.Tags = tagsOf(b.route)

	// ---- handler 函数体: 请求绑定 + 显式参数 ----
	// handler 可能是接口方法（真实仓库 handler 常经接口分发到 service 实现），
	// 此时 FuncDeclOf 返回 nil，需解析到具体实现再扫描其函数体。
	handlerBodies := map[string]*ast.FuncDecl{}
	if fd := b.g.FuncDeclOf(b.route.Handler); fd != nil {
		handlerBodies[b.route.Handler] = fd
	} else {
		for _, impl := range b.g.ConcreteImplsOf(b.route.Handler) {
			if fd := b.g.FuncDeclOf(impl); fd != nil {
				handlerBodies[impl] = fd
			}
		}
	}
	if len(handlerBodies) > 0 {
		// 按符号 ID 排序遍历：参数追加顺序决定输出顺序，map 随机序会破坏确定性。
		bodyIDs := make([]string, 0, len(handlerBodies))
		for symID := range handlerBodies {
			bodyIDs = append(bodyIDs, symID)
		}
		sort.Strings(bodyIDs)
		for _, symID := range bodyIDs {
			b.scanHandlerBody(handlerBodies[symID], symID, &cp)
		}
	} else {
		unknowns = append(unknowns, "handler body not found")
		conf = 0.6
	}

	// 请求体 schema 合成（F4）
	var reqSchemas []*facts.Fact
	if cp.RequestBody != nil && cp.RequestBody.SchemaType != "" {
		reqSchemas = append(reqSchemas, b.schemaFactWith(b.reqSynth, cp.RequestBody.SchemaType))
		if env := cp.RequestBody.EnvelopeType; env != "" && b.g.Type(env) != nil {
			reqSchemas = append(reqSchemas, b.schemaFactWith(b.reqSynth, env))
		}
	}

	// ---- 响应矩阵（调用图追踪，F5） ----
	hits := b.slicer.Trace(b.route.Handler)
	rows, schemaFacts, respConf, respUnknowns := b.responseMatrix(hits)
	cp.Responses = rows
	unknowns = append(unknowns, respUnknowns...)
	conf = minConf(conf, respConf)

	if len(cp.Responses) == 0 {
		unknowns = append(unknowns, "no response sink reached in call graph")
		conf = 0.4
	}

	// 切片内错误码候选（证据注入）：err 变量兜底时把切片里已出现的具体码
	// 收集起来，作为 LLM 的候选目录——LLM 据此缩小到几个候选而非空猜。
	cp.ErrCandidates = errCandidatesOf(b.slicer, b.route.Handler, hits)
	// any 响应字段候选（证据注入）：any 数据槽不可定型时，把切片内已出现的
	// 字段名收集起来，作为 LLM 候选收窄结构。
	cp.DataCandidates = dataCandidatesOf(b.slicer, b.route.Handler, hits)

	// 静态缺口固化到 contract 事实，供档位判定（LLM 兜底的前置输入）。
	cp.Gaps = unknowns
	cp.UnsummarizedWrappers = b.wrapperGaps

	cfact := &facts.Fact{
		ID: "contract:" + b.opKey, Kind: facts.KindContract, Value: cp,
		Source: facts.SourceStatic, Confidence: conf,
		Evidence: append([]facts.Evidence{b.routeEvidence("")}, b.learnedEv...),
		Status:   "verified",
	}

	schemaFacts = append(reqSchemas, schemaFacts...)
	// schema 置信度向 contract 传播（含 unknown 的 schema 拉低整体置信度）。
	// 信封类型除外：其 any 数据槽在成功行/请求体被 allOf 收窄为具体类型，不是本 operation 的缺口。
	envelopes := map[string]bool{}
	if cp.RequestBody != nil && cp.RequestBody.EnvelopeType != "" {
		envelopes["schema:"+cp.RequestBody.EnvelopeType] = true
	}
	for _, r := range cp.Responses {
		if r.EnvelopeType != "" {
			envelopes["schema:"+r.EnvelopeType] = true
		}
	}
	for _, sf := range schemaFacts {
		if envelopes[sf.ID] {
			continue
		}
		if sf.Confidence < conf {
			conf = sf.Confidence
		}
		if sc, ok := sf.Schema(); ok {
			for _, p := range unknownPaths(sc, "") {
				cp.Gaps = append(cp.Gaps, facts.GapSchemaAny(strings.TrimPrefix(sf.ID, "schema:"), p))
			}
		}
	}
	cfact.Value = cp // cp 为值类型：回写 schema 缺口后的最终版本
	cfact.Confidence = conf
	return cfact, schemaFacts, extra
}

// scanHandlerBody 扫描 handler 函数体：按框架原语表（符号匹配）识别请求体绑定、结构体参数绑定与
// 单参数读取；仓库自定义的绑定包装器（DiscoverBinders）优先。
//
// symID 是函数体所属符号的 ID（handler 自身或其具体实现），用于定位类型信息。
// 绑定实参常是上一行 `r := new(mapping.T)` 的局部变量，类型经 info.TypeOf 与局部声明回溯求得。
func (b *ContractPayloadBuilder) scanHandlerBody(fd *ast.FuncDecl, symID string, cp *facts.ContractPayload) {
	info := b.g.TypeInfoOfFunc(symID)
	if info == nil {
		return
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee, _ := codegraph.ResolveCallee(call, info)
		if callee == "" {
			return true
		}
		if bind, ok := b.binders[callee]; ok && cp.RequestBody == nil && bind.Slot < len(call.Args) {
			b.bindRequestBody(call.Args[bind.Slot], info, fd, cp, bind.EnvelopeType, bind.DataField, "wrapper:"+lastSeg(callee))
			return true
		}
		origin := b.prims.origin[callee]
		if bb, ok := b.prims.body[callee]; ok && cp.RequestBody == nil && bb.Arg < len(call.Args) {
			b.bindRequestBody(call.Args[bb.Arg], info, fd, cp, "", "", origin)
			if cp.RequestBody != nil {
				cp.RequestBody.ContentType = bb.ContentType
			}
			b.inlineEnvelope(cp, symID)
			return true
		}
		if sb, ok := b.prims.structs[callee]; ok && sb.Arg < len(call.Args) {
			if t := bodyArgStaticType(call.Args[sb.Arg], info, fd); t != "" {
				if ti := b.g.Type(normalizeTypeString(t)); ti != nil {
					b.expandBinderParams(ti, sb, origin, cp, 0)
				}
			}
			return true
		}
		if pr, ok := b.prims.params[callee]; ok && pr.NameArg < len(call.Args) {
			if name, isLit := stringLit(call.Args[pr.NameArg]); isLit && name != "" && !paramExists(cp.Params, pr.In, name) {
				cp.Params = append(cp.Params, facts.ParamFact{
					In: pr.In, Name: name, Type: pr.Type, Required: pr.In == "path", Origin: origin,
				})
			}
		}
		return true
	})
}

// primIndex 框架原语表按符号索引（多框架合并），并预计算证据来源标签（如 "fiber:Query"）。
type primIndex struct {
	body    map[string]adapter.BodyBinder   // 请求体绑定原语
	structs map[string]adapter.StructBinder // 结构体参数绑定原语
	params  map[string]adapter.ParamReader  // 单参数读取原语
	origin  map[string]string               // 原语符号 → 来源标签 <框架短名>:<方法名>
}

// newPrimIndex 由识别出的框架构造原语索引。
func newPrimIndex(fws []adapter.Framework) *primIndex {
	ix := &primIndex{body: map[string]adapter.BodyBinder{}, structs: map[string]adapter.StructBinder{},
		params: map[string]adapter.ParamReader{}, origin: map[string]string{}}
	for _, fw := range fws {
		p := fw.Primitives()
		label := func(sym string) { ix.origin[sym] = fw.Short() + ":" + lastSeg(sym) }
		for _, x := range p.BodyBinders {
			ix.body[x.Symbol] = x
			label(x.Symbol)
		}
		for _, x := range p.StructBinders {
			ix.structs[x.Symbol] = x
			label(x.Symbol)
		}
		for _, x := range p.ParamReaders {
			ix.params[x.Symbol] = x
			label(x.Symbol)
		}
	}
	return ix
}

// stringLit 字符串字面量的值。
func stringLit(e ast.Expr) (string, bool) {
	if lit, ok := ast.Unparen(e).(*ast.BasicLit); ok && lit.Kind == token.STRING {
		if s, err := strconv.Unquote(lit.Value); err == nil {
			return s, true
		}
	}
	return "", false
}

// jsonContentType 请求体媒体类型（fiber BodyParser 的 JSON 路径）。
const jsonContentType = "application/json"

// bindRequestBody 以绑定实参的静态类型确定请求体；envelopeType 非空时业务体挂在信封 dataField 下。
func (b *ContractPayloadBuilder) bindRequestBody(arg ast.Expr, info *types.Info, fd *ast.FuncDecl,
	cp *facts.ContractPayload, envelopeType, dataField, origin string) {
	t := bodyArgStaticType(arg, info, fd)
	if t == "" {
		return
	}
	typeID := normalizeTypeString(t)
	if typeID == "" {
		return
	}
	cp.RequestBody = &facts.BodyFact{
		ContentType: jsonContentType, SchemaType: typeID,
		EnvelopeType: envelopeType, DataField: dataField, Origin: origin,
	}
}

// inlineEnvelope handler 内联请求信封：直接绑定的类型有 any 字段，且 handler 内对该字段只赋过
// 唯一的具体类型（`p := new(BaseRequestParams); p.Params = params; c.BodyParser(p)`）时，
// 把请求体改写为「信封 + 该字段收窄为业务体」（与绑定包装器的信封同形）。
func (b *ContractPayloadBuilder) inlineEnvelope(cp *facts.ContractPayload, handlerFn string) {
	body := cp.RequestBody
	if body == nil || body.EnvelopeType != "" {
		return
	}
	ti := b.g.Type(body.SchemaType)
	if ti == nil || !ti.IsStruct {
		return
	}
	for _, f := range ti.Fields {
		if f.Kind != "any" {
			continue
		}
		ct := uniqueConcrete(b.slicer.FieldAssignTypes(handlerFn, body.SchemaType, f.Name))
		if ct == nil {
			continue
		}
		if p, ok := ct.(*types.Pointer); ok {
			ct = p.Elem()
		}
		inner := codegraph.TypeIDOf(ct)
		if b.g.Type(inner) == nil {
			continue
		}
		body.EnvelopeType, body.DataField, body.SchemaType = body.SchemaType, f.JSONName, inner
		return
	}
}

// maxBinderEmbedDepth 绑定结构体嵌入展开的最大深度（防御嵌入环）。
const maxBinderEmbedDepth = 4

// tagSkip struct tag 值为 "-" 表示该字段不参与绑定。
const tagSkip = "-"

// expandBinderParams 将绑定目标结构体的字段展开为参数：名取 tag（缺省为字段名，与 fiber 解码器一致），
// "-" 跳过，未命名的嵌入结构体递归展平；类型/枚举/必填复用 schema 合成规则。
func (b *ContractPayloadBuilder) expandBinderParams(ti *codegraph.TypeInfo, bind adapter.StructBinder,
	origin string, cp *facts.ContractPayload, depth int) {
	if ti == nil || !ti.IsStruct || depth > maxBinderEmbedDepth {
		return
	}
	for _, f := range ti.Fields {
		tagVal, hasTag := reflect.StructTag(f.Tag).Lookup(bind.TagKey)
		name, _, _ := strings.Cut(tagVal, ",")
		if name == tagSkip {
			continue
		}
		if f.Embedded && name == "" {
			b.expandBinderParams(b.g.Type(f.TypeID), bind, origin, cp, depth+1)
			continue
		}
		if !token.IsExported(f.Name) {
			continue // fiber 绑定器（schema 解码）只写导出字段
		}
		if !hasTag || name == "" {
			name = f.Name
		}
		if paramExists(cp.Params, bind.In, name) {
			continue
		}
		required, _ := codegraph.ConstraintRequired(f.Tag)
		pf := facts.ParamFact{
			In: bind.In, Name: name, Required: required || bind.In == "path",
			Description: f.Doc, Origin: origin,
		}
		sc := b.synth.FieldSchema(f)
		pf.Type, pf.Format, pf.Enum = paramScalar(sc)
		if sc.Type == "array" && sc.Items != nil {
			pf.Items, _, _ = paramScalar(sc.Items)
		}
		cp.Params = append(cp.Params, pf)
	}
}

// paramScalar 参数 schema → (type, format, enum)；参数只能承载标量或标量数组，
// 对象/未知类型按 string 输出（查询串原文）。
func paramScalar(sc *typeschema.Schema) (string, string, []string) {
	if sc == nil {
		return "string", "", nil
	}
	switch sc.Type {
	case "string", "integer", "number", "boolean", "array":
		return sc.Type, sc.Format, sc.Enum
	}
	return "string", "", sc.Enum
}

// responseMatrix 响应矩阵合成（成功信封 + 错误码行 + 未解析行）。
func (b *ContractPayloadBuilder) responseMatrix(hits []slicing.SinkHit) (
	[]facts.ResponseFact, []*facts.Fact, float64, []string) {

	var rows []facts.ResponseFact
	var schemaFacts []*facts.Fact
	conf := 1.0
	var unknowns []string

	envSpec := b.prof.ResponseEnvelope
	successCode := 0
	if envSpec != nil {
		successCode = envSpec.SuccessCode
	}

	seenSuccess := map[string]bool{}
	narrowed := map[string]*facts.Fact{} // 收窄 schema 类型 ID → 事实
	seenErrCode := map[string]bool{}
	var errSources []string
	hasUnresolvedErr := false
	unresolvedEnv := "" // 未解析错误行所用的真实信封类型

	for _, h := range hits {
		sinkLoc := fmt.Sprintf("%s:%d", shortFile(h.Site.File), h.Site.Line)
		switch {
		case h.Success:
			// 值级收窄：data 由字面量构造且 any 部分可由字面量/字段赋值定型时，换成 operation 专属 schema。
			if h.DataInline != nil {
				nf := b.inlineDataFact(h)
				h.DataTypeID = strings.TrimPrefix(nf.ID, "schema:")
				h.DataUnknown, h.HasBody = false, true
				narrowed[h.DataTypeID] = nf
			} else if !h.DataUnion && (h.DataUnknown || h.DataTypeID != "") && !strings.HasPrefix(h.DataTypeID, "[]") {
				if nf, ok := b.narrowData(h); ok {
					h.DataTypeID = strings.TrimPrefix(nf.ID, "schema:")
					h.DataUnknown, h.DataMapValue, h.HasBody = false, "", true
					narrowed[h.DataTypeID] = nf
				}
			}
			// 成功行（原生写出位于 if err != nil 分支时为失败行）: 信封码 + data schema
			code := successCode
			if h.FixedCode != nil {
				code = *h.FixedCode
			}
			failure := h.Raw && h.ErrorBranch
			b.noteLearned(h, &conf)
			if h.ParamFed && !containsStr(b.wrapperGaps, h.Site.Caller) {
				// 写出的响应体取自所在函数形参，却没被识别为包装器：调用点的实参类型丢了，必须显式报缺口。
				b.wrapperGaps = append(b.wrapperGaps, h.Site.Caller)
				unknowns = append(unknowns, facts.GapWrapperPrefix+h.Site.Caller)
				conf = minConf(conf, unsummarizedWrapperConfidence)
			}
			key := fmt.Sprintf("%s|%d|%v", h.DataTypeID, code, failure)
			if seenSuccess[key] {
				// 合并 sink 证据到既有行
				for i := range rows {
					if rows[i].Envelope != nil && rows[i].Envelope.Code == code && rows[i].SchemaType == h.DataTypeID && rows[i].Failure == failure {
						rows[i].Sink = rows[i].Sink + "," + sinkLoc
					}
				}
				continue
			}
			seenSuccess[key] = true
			row := facts.ResponseFact{
				Status: h.Status, HasBody: h.HasBody,
				Envelope:     &facts.Envelope{Code: code},
				Sink:         sinkLoc,
				EnvelopeType: h.EnvelopeType, DataField: h.DataField, Raw: h.Raw, Failure: failure,
			}
			schemaFacts = append(schemaFacts, b.envelopeFact(h.EnvelopeType)...)
			if h.DataTypeID != "" {
				row.SchemaType = h.DataTypeID
				if strings.HasPrefix(h.DataTypeID, "[]") {
					// 切片响应: 元素进 components，数组包装在编译期展开（§7.1）
					elem := strings.TrimPrefix(h.DataTypeID, "[]")
					if b.g.Type(elem) != nil {
						schemaFacts = append(schemaFacts, b.schemaFactAt(elem, h.Site))
					}
				} else if nf := narrowed[h.DataTypeID]; nf != nil {
					schemaFacts = append(schemaFacts, nf)
				} else {
					schemaFacts = append(schemaFacts, b.schemaFactAt(h.DataTypeID, h.Site))
				}
			} else if h.DataMapValue != "" {
				// map[K]V 且 V 为基础类型：静态定型为 additionalProperties map（§7.2.3），
				// 不进 components，编译期直接展开。
				row.MapValueType = h.DataMapValue
			}
			if h.DataUnknown {
				row.SchemaType = ""
				unknowns = append(unknowns, facts.GapSuccessAny)
				conf = minConf(conf, 0.6)
			}
			rows = append(rows, row)
		case h.FixedCode != nil:
			// 包装器失败分支的固定信封码（如 gin.H{"code": 500, "msg": err.Error()}）
			b.noteLearned(h, &conf)
			key := fmt.Sprintf("fixed:%d:%s", *h.FixedCode, h.EnvelopeType)
			if seenErrCode[key] {
				for i := range rows {
					if rows[i].Envelope != nil && rows[i].Envelope.CodeRef == fixedCodeRef && rows[i].Envelope.Code == *h.FixedCode {
						rows[i].Sink = rows[i].Sink + "," + sinkLoc
					}
				}
				continue
			}
			seenErrCode[key] = true
			rows = append(rows, facts.ResponseFact{
				Status: h.Status, HasBody: false,
				Envelope:     &facts.Envelope{Code: *h.FixedCode, CodeRef: fixedCodeRef, Msg: "fixed code in response wrapper"},
				Sink:         sinkLoc,
				EnvelopeType: h.EnvelopeType,
			})
			schemaFacts = append(schemaFacts, b.envelopeFact(h.EnvelopeType)...)
		case h.ErrConstID != "":
			// 具体错误码行（可溯源到常量）
			entry, ok := b.catalog[h.ErrConstID]
			if !ok {
				entry = slicing.ErrorCodeEntry{Symbol: h.ErrConstID, Code: -1, Msg: "unknown"}
			}
			if seenErrCode[h.ErrConstID] {
				for i := range rows {
					if rows[i].Envelope != nil && rows[i].Envelope.CodeRef == h.ErrConstID {
						rows[i].Sink = rows[i].Sink + "," + sinkLoc
					}
				}
				continue
			}
			seenErrCode[h.ErrConstID] = true
			rows = append(rows, facts.ResponseFact{
				Status: h.Status, HasBody: false,
				Envelope:     &facts.Envelope{Code: entry.Code, CodeRef: h.ErrConstID, Msg: entry.Msg},
				Sink:         sinkLoc,
				EnvelopeType: h.EnvelopeType,
			})
			schemaFacts = append(schemaFacts, b.envelopeFact(h.EnvelopeType)...)
		case h.ErrUncoded || h.ErrDynamic:
			// 静态定性的错误行（不带业务码 / 动态码）：每类一行，写出点与来源证据合并。
			code, ref, msg := facts.UncodedCode, facts.CodeRefUncoded, uncodedErrMsg
			if h.ErrDynamic {
				code, ref, msg = facts.DynamicCode, facts.CodeRefDynamic, dynamicCodeMsg
			}
			if i := rowWithCode(rows, code); i >= 0 {
				rows[i].Sink = rows[i].Sink + "," + sinkLoc
				rows[i].ErrSource = mergeSites(rows[i].ErrSource, h.ErrSource)
				continue
			}
			rows = append(rows, facts.ResponseFact{
				Status: h.Status, HasBody: false,
				Envelope:     &facts.Envelope{Code: code, CodeRef: ref, Msg: msg},
				Sink:         sinkLoc,
				ErrSource:    h.ErrSource,
				EnvelopeType: h.EnvelopeType,
			})
			schemaFacts = append(schemaFacts, b.envelopeFact(h.EnvelopeType)...)
		case h.ErrUnresolved:
			hasUnresolvedErr = true
			if unresolvedEnv == "" {
				unresolvedEnv = h.EnvelopeType
			}
			// 溯源成功但码不可静态定（如 errgroup.Wait）：把来源记进 unknown，供报告展示。
			if h.ErrSource != "" && !containsStr(errSources, h.ErrSource) {
				errSources = append(errSources, h.ErrSource)
			}
		}
	}
	if hasUnresolvedErr {
		msg := "error variable not statically traceable"
		if len(errSources) > 0 {
			msg += " (sources: " + strings.Join(errSources, ", ") + ")"
		}
		rows = append(rows, facts.ResponseFact{
			Status: hits[0].Status, HasBody: false,
			Envelope:     &facts.Envelope{Code: facts.UnresolvedCode, CodeRef: "unresolved", Msg: msg},
			Sink:         "(multiple)",
			ErrSource:    strings.Join(errSources, ", "),
			EnvelopeType: unresolvedEnv,
		})
		schemaFacts = append(schemaFacts, b.envelopeFact(unresolvedEnv)...)
		unknowns = append(unknowns, facts.GapErrorUnresolved)
		conf = minConf(conf, 0.75)
	}

	// 稳定排序: 成功在前，错误码升序，未解析最后
	facts.SortResponses(rows)
	return rows, schemaFacts, conf, unknowns
}

// 静态定性错误行的信封描述（编译进响应描述）。
const (
	uncodedErrMsg  = "error without business code, serialized as-is"
	dynamicCodeMsg = "business code taken from a runtime value"
)

// rowWithCode 响应行中首个信封码为 code 的下标；没有返回 -1。
func rowWithCode(rows []facts.ResponseFact, code int) int {
	for i := range rows {
		if rows[i].Envelope != nil && rows[i].Envelope.Code == code {
			return i
		}
	}
	return -1
}

// mergeSites 合并两个逗号分隔的来源点串（去重、有序，截断到 maxMergedSites）。
func mergeSites(a, b string) string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(facts.SplitSites(a), facts.SplitSites(b)...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	if len(out) > maxMergedSites {
		out = out[:maxMergedSites]
	}
	return facts.JoinSites(out)
}

// maxMergedSites 一个静态定性错误行合并后最多保留的来源点数（控制产物体积）。
const maxMergedSites = 8

// noteLearned 响应行来自 LLM 包装器摘要时：置信度按 symbol 级核对封顶，并记录包装器声明为证据。
func (b *ContractPayloadBuilder) noteLearned(h slicing.SinkHit, conf *float64) {
	if !h.Learned {
		return
	}
	*conf = minConf(*conf, facts.VerificationCap(facts.VerifySymbol))
	sym := b.g.Sym(h.Site.Callee)
	if sym == nil {
		return
	}
	for _, ev := range b.learnedEv {
		if ev.File == sym.File && ev.StartLine == sym.Line {
			return
		}
	}
	b.learnedEv = append(b.learnedEv, facts.Evidence{File: sym.File, StartLine: sym.Line, EndLine: sym.Line,
		BlobSHA: b.g.FileHashOf(sym.File), Quote: "llm:wrapper-summary " + lastSeg(h.Site.Callee)})
}

// fixedCodeRef 固定信封码行的 CodeRef（码来自包装器字面量，不对应错误码常量）。
const fixedCodeRef = "literal"

// unsummarizedWrapperConfidence 存在未识别响应包装器时 operation 的置信度上限。
const unsummarizedWrapperConfidence = 0.75

// syntheticEnvelopeFact map 字面量信封（包装器摘要合成）的 schema 事实：data 键为 any（成功行 allOf 收窄），
// 其余键取值的静态类型；证据为包装器函数声明。
func (b *ContractPayloadBuilder) syntheticEnvelopeFact(env slicing.SyntheticEnvelope) *facts.Fact {
	sc := &schema.Schema{Type: "object"}
	for _, f := range env.Fields {
		var fs *schema.Schema
		switch {
		case f.Role == slicing.RoleData:
			fs = &schema.Schema{Description: "business payload (narrowed per operation)"}
		case f.Type != nil:
			fs = b.synth.SchemaOf(types.Default(f.Type))
		case f.JSONType != "":
			fs = &schema.Schema{Type: f.JSONType} // LLM 包装器摘要给出的类型（封闭集合）
		default:
			fs = &schema.Schema{Unknown: true, UnknownWhy: "envelope value type unknown"}
		}
		sc.Props = append(sc.Props, schema.Prop{Name: f.Key, Schema: fs, Required: f.Required})
		if f.Required {
			sc.Required = append(sc.Required, f.Key)
		}
	}
	ev := facts.NewEvidence(facts.SourceStatic, b.evFile, b.route.Line, b.route.Line, b.blob, "")
	if sym := b.g.Sym(env.Fn); sym != nil {
		ev = facts.NewEvidence(facts.SourceStatic, sym.File, sym.Line, sym.Line, b.g.FileHashOf(sym.File), "")
	}
	return &facts.Fact{
		ID: "schema:" + env.ID, Kind: facts.KindSchema, Value: facts.SchemaPayload{Schema: sc},
		Source: facts.SourceStatic, Confidence: 1.0, Evidence: []facts.Evidence{ev}, Status: "verified",
	}
}

// envelopeFact 真实信封类型的 schema 事实（无信封类型时为空）。
func (b *ContractPayloadBuilder) envelopeFact(typeID string) []*facts.Fact {
	if env, ok := b.envs[typeID]; ok {
		return []*facts.Fact{b.syntheticEnvelopeFact(env)}
	}
	if typeID == "" || b.g.Type(typeID) == nil {
		return nil
	}
	return []*facts.Fact{b.schemaFact(typeID)}
}

// schemaFact 响应方向 schema 事实。
func (b *ContractPayloadBuilder) schemaFact(typeID string) *facts.Fact {
	return b.schemaFactWith(b.synth, typeID)
}

// schemaFactAt 响应方向 schema 事实；仓库外类型以写出点 site 为使用证据（比路由注册点更精确）。
func (b *ContractPayloadBuilder) schemaFactAt(typeID string, site codegraph.CallSite) *facts.Fact {
	f := b.schemaFact(typeID)
	if b.g.Type(typeID) == nil && site.File != "" {
		f.Evidence = []facts.Evidence{{File: site.File, StartLine: site.Line, EndLine: site.Line,
			BlobSHA: b.g.FileHashOf(site.File)}}
	}
	return f
}

// schemaFactWith 用指定方向的合成器产出 schema 事实（请求/响应必填语义不同）。
func (b *ContractPayloadBuilder) schemaFactWith(synth *typeschema.Synthesizer, typeID string) *facts.Fact {
	if synth == nil {
		synth = b.synth
	}
	sc := synth.Synthesize(typeID)
	ev := []facts.Evidence{}
	if ti := b.g.Type(typeID); ti != nil {
		ev = append(ev, facts.Evidence{
			File: ti.File, StartLine: ti.Line, EndLine: ti.Line,
			BlobSHA: b.g.FileHashOf(ti.File),
		})
	} else {
		// 仓库外类型（如 fiber.Map）无声明可引：以本 operation 的使用点为证据。
		ev = append(ev, b.routeEvidence(""))
	}
	return &facts.Fact{
		ID: "schema:" + typeID, Kind: facts.KindSchema, Value: facts.SchemaPayload{Schema: sc},
		Source: facts.SourceStatic, Confidence: schemaConfidence(sc),
		Evidence: ev, Status: "verified",
	}
}

func (b *ContractPayloadBuilder) routeEvidence(mw string) facts.Evidence {
	ev := facts.Evidence{File: b.evFile, StartLine: b.route.Line, EndLine: b.route.Line, BlobSHA: b.blob}
	if mw != "" {
		if sym := b.g.Sym(mw); sym != nil {
			ev = facts.Evidence{File: sym.File, StartLine: sym.Line, EndLine: sym.Line,
				BlobSHA: b.g.FileHashOf(sym.File)}
		}
	}
	return ev
}

func (b *ContractPayloadBuilder) routedScheme(name string) *facts.Fact { return nil }

// ---- 辅助 --------------------------------------------------------------

// bodyArgStaticType 求 BodyParser 实参的静态类型。
//
// 真实仓库 handler 的惯用写法是 `r := new(mapping.T); c.BodyParser(r)`，
// 实参是局部变量标识符。go/types 的 info.TypeOf(ident) 比 info.Types[ident]
// 更可靠（标识符在 Types map 里常缺失），故对 *ast.Ident 走 TypeOf 兜底；
// 若仍为空，再沿函数体回溯 `ident := new(T)` / `ident := &T{}` 声明。
func bodyArgStaticType(arg ast.Expr, info *types.Info, fd *ast.FuncDecl) string {
	if info == nil {
		return ""
	}
	arg = ast.Unparen(arg)
	switch e := arg.(type) {
	case *ast.Ident:
		if t := info.TypeOf(e); t != nil {
			return t.String()
		}
		// 兜底：回溯函数体里的局部声明。
		if fd != nil && fd.Body != nil {
			if t := findLocalDeclType(fd.Body, e.Name); t != "" {
				return t
			}
		}
		return ""
	case *ast.UnaryExpr:
		if e.Op.String() == "&" {
			return bodyArgStaticType(e.X, info, fd)
		}
	case *ast.CallExpr:
		// new(T)
		if id, ok := e.Fun.(*ast.Ident); ok && id.Name == "new" && len(e.Args) == 1 {
			return bodyArgStaticType(e.Args[0], info, fd)
		}
	}
	if t := info.TypeOf(arg); t != nil {
		return t.String()
	}
	return ""
}

// findLocalDeclType 在函数体中查找 `name := new(T)` / `name := &T{}` / `name := T{}`
// 的局部声明，返回 T 的静态类型串。找不到返回空。
func findLocalDeclType(body *ast.BlockStmt, name string) string {
	var found string
	ast.Inspect(body, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != name || i >= len(as.Rhs) {
				continue
			}
			rhs := as.Rhs[i]
			// new(T)
			if call, ok := ast.Unparen(rhs).(*ast.CallExpr); ok && len(call.Args) == 1 {
				if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "new" {
					if tv, ok := typeOfTypeExpr(call.Args[0], nil); ok {
						found = tv
						return false
					}
				}
			}
			// &T{} / T{}
			expr := rhs
			if u, ok := rhs.(*ast.UnaryExpr); ok && u.Op.String() == "&" {
				expr = u.X
			}
			if cl, ok := expr.(*ast.CompositeLit); ok {
				if tv, ok := typeOfTypeExpr(cl.Type, nil); ok {
					found = tv
					return false
				}
			}
		}
		return true
	})
	return found
}

// typeOfTypeExpr 求一个「类型表达式」的静态类型串（无 *types.Info 时退化为
// 语法层解析：Ident → 名称，SelectorExpr → pkg.Name）。返回 (类型串, 是否成功)。
func typeOfTypeExpr(t ast.Expr, _ *types.Info) (string, bool) {
	switch e := ast.Unparen(t).(type) {
	case *ast.Ident:
		return e.Name, true
	case *ast.SelectorExpr:
		if x, ok := e.X.(*ast.Ident); ok {
			return x.Name + "." + e.Sel.Name, true
		}
	}
	return "", false
}

func normalizeTypeString(t string) string {
	t = strings.TrimPrefix(t, "*")
	if i := strings.Index(t, "["); i > 0 && strings.HasSuffix(t, "]") {
		t = t[:i]
	}
	if strings.ContainsAny(t, " ()[]") || t == "" || isBasicType(t) {
		return ""
	}
	return t
}

func isBasicType(t string) bool {
	switch t {
	case "string", "int", "int64", "int32", "float64", "float32", "bool", "byte", "any", "interface{}":
		return true
	}
	return false
}

func paramExists(ps []facts.ParamFact, in, name string) bool {
	for _, p := range ps {
		if p.In == in && p.Name == name {
			return true
		}
	}
	return false
}

func middlewareKey(mwSymbol string) string {
	return lastSeg(mwSymbol)
}

func schemeNameOf(m profile.SecurityMapping) string {
	if m.Header != "" {
		return m.Header
	}
	return "auth"
}

func scopeArgOf(r adapter.Route, mw string) string {
	// PermissionCheck("0xff") 的字面量实参 → scope（引擎侧简化为包内解析）
	return ""
}

// tagsOf operation 分组标签：方法 handler 取接收者类型名，包级函数 handler 取包路径末段。
func tagsOf(r adapter.Route) []string {
	if r.Handler == "" {
		return nil
	}
	if seg := handlerOwner(r.Handler); seg != "" {
		return []string{seg}
	}
	return nil
}

// handlerOwner handler 的归属名："pkg.(*pkg.Recv).Method" → "Recv"；"example.com/x/api.Func" → "api"。
func handlerOwner(handlerID string) string {
	if i := strings.Index(handlerID, "(*"); i >= 0 {
		if j := strings.Index(handlerID[i:], ")."); j > 0 {
			return lastSeg(handlerID[i+2 : i+j])
		}
	}
	pkg := handlerID
	if i := strings.LastIndex(handlerID, "."); i > strings.LastIndex(handlerID, "/") {
		pkg = handlerID[:i]
	}
	return pkg[strings.LastIndex(pkg, "/")+1:]
}

func operationIDOf(r adapter.Route) string {
	name := lastSeg(r.Handler)
	if name == "" {
		name = r.Method + strings.ReplaceAll(r.Path, "/", "")
	}
	if len(name) > 0 {
		name = strings.ToLower(name[:1]) + name[1:]
	}
	return name
}

func shortFile(f string) string {
	if i := strings.LastIndex(f, "/"); i >= 0 {
		return f[i+1:]
	}
	return f
}

func lastSeg(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

func minConf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// errCandidatesOf 把切片内错误码候选转成 fact 载荷（证据注入）。
// CollectErrCandidates 已按常量名去重排序，这里直接映射为可序列化形态。
func errCandidatesOf(slicer *slicing.Slicer, handlerID string, hits []slicing.SinkHit) []facts.ErrCandidateFact {
	cands := slicer.CollectErrCandidates(handlerID, hits)
	out := make([]facts.ErrCandidateFact, 0, len(cands))
	for _, c := range cands {
		out = append(out, facts.ErrCandidateFact{
			Symbol: c.Symbol, Name: c.Name, Code: c.Code,
		})
	}
	return out
}

// dataCandidatesOf 把切片内数据字段候选转成 fact 载荷（证据注入）。
func dataCandidatesOf(slicer *slicing.Slicer, handlerID string, hits []slicing.SinkHit) []facts.DataCandidateFact {
	cands := slicer.CollectDataCandidates(handlerID, hits)
	out := make([]facts.DataCandidateFact, 0, len(cands))
	for _, c := range cands {
		out = append(out, facts.DataCandidateFact{Name: c.Name})
	}
	return out
}

// unknownPaths schema 内不可静态定型字段的 JSON 路径（报告据此说明置信度为何被拉低）。
func unknownPaths(sc *typeschema.Schema, prefix string) []string {
	if sc == nil {
		return nil
	}
	if sc.Unknown {
		if prefix == "" {
			return []string{"<root>"}
		}
		return []string{prefix}
	}
	out := unknownPaths(sc.Items, prefix+"[]")
	for _, p := range sc.Props {
		path := p.Name
		if prefix != "" {
			path = prefix + "." + p.Name
		}
		out = append(out, unknownPaths(p.Schema, path)...)
	}
	return out
}

func schemaConfidence(sc *typeschema.Schema) float64 {
	if sc == nil {
		return 0.5
	}
	if sc.Unknown {
		return 0.6
	}
	// 递归检查内层 unknown
	var hasUnknown func(s *typeschema.Schema) bool
	hasUnknown = func(s *typeschema.Schema) bool {
		if s == nil {
			return false
		}
		if s.Unknown {
			return true
		}
		if s.Items != nil && hasUnknown(s.Items) {
			return true
		}
		for _, p := range s.Props {
			if hasUnknown(p.Schema) {
				return true
			}
		}
		return false
	}
	if hasUnknown(sc) {
		return 0.75
	}
	return 1.0
}

var _ = types.Typ

func containsStr(arr []string, v string) bool {
	for _, x := range arr {
		if x == v {
			return true
		}
	}
	return false
}
