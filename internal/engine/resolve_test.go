package engine

import (
	"testing"

	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/infer"
	"github.com/specforge/specforge/internal/loader"
	"github.com/specforge/specforge/internal/slicing"
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
	applyGapResolution(cp, r, catalog)
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
	applyGapResolution(cp, r, catalog)
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
