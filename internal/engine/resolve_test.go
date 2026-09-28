package engine

import (
	"testing"

	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/infer"
	"github.com/specforge/specforge/internal/loader"
	"github.com/specforge/specforge/internal/profile"
	"github.com/specforge/specforge/internal/slicing"
	"github.com/specforge/specforge/internal/typeschema"
)

// mockProvider 返回预设 JSON 的 Provider，用于离线测试 resolveGaps。
type mockProvider struct{ out string }

func (m *mockProvider) Name() string { return "mock" }
func (m *mockProvider) Complete(system, prompt string, schema []byte) ([]byte, error) {
	return []byte(m.out), nil
}

// TestApplyGapResolutionRejectsHallucinatedCode 幻觉 codeRef 被丢弃。
func TestApplyGapResolutionRejectsHallucinatedCode(t *testing.T) {
	catalog := map[string]slicing.ErrorCodeEntry{
		"trade/pkg/code.ErrDatabase": {Code: 100011, Msg: "数据库错误"},
	}
	cp := &facts.ContractPayload{
		Responses: []facts.ResponseFact{
			{Status: 200, Envelope: &facts.Envelope{Code: -1, CodeRef: "unresolved"}},
		},
	}
	r := &infer.GapResolution{
		ErrorCodes: []infer.ResolvedError{{Status: 200, Code: 999999, CodeRef: "trade/pkg/code.ErrFake"}},
	}
	applyGapResolution(cp, r, catalog, nil, 0)
	if cp.Responses[0].Envelope.Code != -1 {
		t.Fatalf("hallucinated codeRef should be dropped, got code=%d", cp.Responses[0].Envelope.Code)
	}
}

// TestApplyGapResolutionWritesBackValidCode 命中目录的 codeRef 写回。
func TestApplyGapResolutionWritesBackValidCode(t *testing.T) {
	catalog := map[string]slicing.ErrorCodeEntry{
		"trade/pkg/code.ErrDatabase": {Code: 100011, Msg: "数据库错误"},
	}
	cp := &facts.ContractPayload{
		Responses: []facts.ResponseFact{
			{Status: 200, Envelope: &facts.Envelope{Code: -1, CodeRef: "unresolved"}},
		},
	}
	r := &infer.GapResolution{
		ErrorCodes: []infer.ResolvedError{{Status: 200, Code: 100011, CodeRef: "ErrDatabase"}},
	}
	applyGapResolution(cp, r, catalog, nil, 0)
	row := cp.Responses[0]
	if row.Envelope.Code != 100011 {
		t.Fatalf("want code=100011, got %d", row.Envelope.Code)
	}
	if row.Source != "llm" {
		t.Fatalf("want Source=llm, got %q", row.Source)
	}
}

// TestResolveGapsOffline 无 provider 时返回 ErrNoProvider。
func TestResolveGapsOffline(t *testing.T) {
	if _, err := infer.ResolveGaps(nil, infer.GapTask{}); err != infer.ErrNoProvider {
		t.Fatalf("want ErrNoProvider, got %v", err)
	}
}

// TestApplyResponseSchemaWritesBackFieldInWhitelist 命中代码图字段白名单的 any
// 响应 schema 被写回：成功行指向新 schema，新增 KindSchema 事实。
func TestApplyResponseSchemaWritesBackFieldInWhitelist(t *testing.T) {
	g := &codegraph.Graph{Types: map[string]*codegraph.TypeInfo{
		"repo/pkg/mapping.Resp": {
			IsStruct: true,
			Fields: []codegraph.Field{
				{Name: "OrderID", JSONName: "order_id", TypeStr: "string"},
				{Name: "Amount", JSONName: "amount", TypeStr: "float64"},
			},
		},
	}}
	cp := &facts.ContractPayload{
		OperationID: "orderCheck",
		Responses: []facts.ResponseFact{
			{Status: 200, Envelope: &facts.Envelope{Code: 0}},
		},
		DataCandidates: []facts.DataCandidateFact{
			{Name: "order_id"}, {Name: "amount"},
		},
	}
	rs := &infer.ResolvedSchema{Properties: []infer.ResolvedProp{
		{Name: "order_id", Type: "string", Required: true},
		{Name: "amount", Type: "number"},
	}}
	f := applyResponseSchema(cp, rs, g, 0)
	if f == nil {
		t.Fatal("want schema fact, got nil")
	}
	if cp.Responses[0].SchemaType != "llm:any:orderCheck" {
		t.Fatalf("SchemaType = %q, want llm:any:orderCheck", cp.Responses[0].SchemaType)
	}
	if cp.Responses[0].Source != "llm" {
		t.Fatalf("Source = %q, want llm", cp.Responses[0].Source)
	}
	sc, ok := f.Value.(*typeschema.Schema)
	if !ok {
		t.Fatalf("fact value = %T, want *typeschema.Schema", f.Value)
	}
	if len(sc.Props) != 2 || sc.Props[0].Name != "order_id" {
		t.Fatalf("props = %+v", sc.Props)
	}
	// 类型从代码图反查（float64 → number），而非信任 LLM 报的 "number"。
	if sc.Props[1].Schema.Type != "number" {
		t.Fatalf("amount type = %q, want number", sc.Props[1].Schema.Type)
	}
}

// TestApplyResponseSchemaRejectsHallucinatedField 字段名不在代码图白名单内，
// 整条 responseSchema 丢弃（防幻觉）。
func TestApplyResponseSchemaRejectsHallucinatedField(t *testing.T) {
	g := &codegraph.Graph{Types: map[string]*codegraph.TypeInfo{
		"repo/pkg/mapping.Resp": {
			IsStruct: true,
			Fields:   []codegraph.Field{{Name: "OrderID", JSONName: "order_id", TypeStr: "string"}},
		},
	}}
	cp := &facts.ContractPayload{
		OperationID: "orderCheck",
		Responses: []facts.ResponseFact{
			{Status: 200, Envelope: &facts.Envelope{Code: 0}},
		},
		DataCandidates: []facts.DataCandidateFact{
			{Name: "order_id"},
		},
	}
	rs := &infer.ResolvedSchema{Properties: []infer.ResolvedProp{
		{Name: "phantom_field", Type: "string"},
	}}
	if f := applyResponseSchema(cp, rs, g, 0); f != nil {
		t.Fatal("want nil (hallucinated field dropped), got fact")
	}
	if cp.Responses[0].SchemaType != "" {
		t.Fatalf("SchemaType should stay empty, got %q", cp.Responses[0].SchemaType)
	}
}

// TestBuildToolsReadSymbol 工具闭包能读符号。
func TestBuildToolsReadSymbol(t *testing.T) {
	l, _ := loader.LoadRepo("/home/whj/projects/sample-ipo-rebase")
	pkgs := l.ServiceFilter("service-ipo")
	g, _ := codegraph.Build(pkgs, l.Fset)
	tools := buildTools(g)
	if len(tools) != 3 {
		t.Fatalf("want 3 tools, got %d", len(tools))
	}
	// read_symbol 对已知符号应返回 JSON。
	var readSym, calleesT, readType infer.ToolSpec
	for _, t := range tools {
		switch t.Name {
		case "read_symbol":
			readSym = t
		case "callees":
			calleesT = t
		case "read_type":
			readType = t
		}
	}
	_ = readType
	if readSym.Call == nil || calleesT.Call == nil {
		t.Fatal("read_symbol / callees tools missing Call")
	}
	out, err := readSym.Call(map[string]any{"id": "trade/internal/common/app.ping"})
	if err != nil {
		t.Fatalf("read_symbol call: %v", err)
	}
	if len(out) == 0 || out[0] != '{' {
		t.Fatalf("read_symbol should return JSON, got %q", out)
	}
	out2, err := calleesT.Call(map[string]any{"id": "trade/internal/common/app.ping"})
	if err != nil {
		t.Fatalf("callees call: %v", err)
	}
	if len(out2) == 0 {
		t.Fatal("callees should return JSON")
	}
}

// TestMergeProfileCandidateFillsMissing 画像候选填补缺失（framework / sinks）。
func TestMergeProfileCandidateFillsMissing(t *testing.T) {
	prof := profile.Default()
	cand := &infer.ProfileCandidate{
		Framework: "gofiber/v2",
		ResponseSinks: []infer.SinkCandidate{
			{Symbol: "code.WriteResponse", Signature: "(c,err,data)", DataSlot: 2, ErrSlot: 1, Status: 200},
		},
	}
	merged := mergeProfileCandidate(prof, cand)
	if merged.Framework != "gofiber/v2" {
		t.Fatalf("framework = %q, want gofiber/v2", merged.Framework)
	}
	if len(merged.ResponseSinks) != 1 || merged.ResponseSinks[0].Symbol != "code.WriteResponse" {
		t.Fatalf("sinks = %+v", merged.ResponseSinks)
	}
}

// TestMergeProfileCandidateDoesNotOverride 画像候选不覆盖既有约定。
func TestMergeProfileCandidateDoesNotOverride(t *testing.T) {
	prof := &profile.Profile{
		Framework: "gofiber/v2",
		ResponseSinks: []profile.SinkPattern{
			{Symbol: "existing.Sink", DataSlot: 2, ErrSlot: 1, Status: 200},
		},
		AuthMiddleware: map[string]profile.SecurityMapping{},
	}
	cand := &infer.ProfileCandidate{
		Framework: "gin",
		ResponseSinks: []infer.SinkCandidate{
			{Symbol: "llm.Sink", DataSlot: 1},
		},
	}
	merged := mergeProfileCandidate(prof, cand)
	if merged.Framework != "gofiber/v2" {
		t.Fatalf("framework should not be overridden, got %q", merged.Framework)
	}
	if len(merged.ResponseSinks) != 1 || merged.ResponseSinks[0].Symbol != "existing.Sink" {
		t.Fatalf("sinks should not be overridden, got %+v", merged.ResponseSinks)
	}
}
