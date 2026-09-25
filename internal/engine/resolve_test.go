package engine

import (
	"testing"

	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/infer"
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
