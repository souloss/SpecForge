package golang

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/frontend"
	"github.com/specforge/specforge/internal/infer"
)

// fixedProvider 按 system prompt 前缀返回预设输出的 Provider（包装器摘要之外的调用返回空对象）。
type fixedProvider struct {
	wrapper string // 包装器摘要的输出
	calls   int    // 包装器摘要调用次数
}

func (f *fixedProvider) Name() string { return "fixed" }
func (f *fixedProvider) Complete(_ context.Context, req infer.Request) (infer.Response, error) {
	if strings.HasPrefix(req.System, "You analyze one helper function") {
		f.calls++
		return infer.Response{Text: []byte(f.wrapper)}, nil
	}
	return infer.Response{Text: []byte(`{}`)}, nil
}

// analyzeGin 用给定 provider 分析 gin 样本；gin 模块不可用时跳过。
func analyzeGin(t *testing.T, p infer.Provider) *frontend.Analysis {
	t.Helper()
	repo := filepath.Join("..", "..", "..", "testdata", "gin-repo")
	an, err := New().Analyze(context.Background(), frontend.Request{
		RepoDir: repo, Provider: p, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Stage: func(string) func() { return func() {} },
	})
	if err != nil || len(an.Handlers) == 0 {
		t.Skipf("gin sample not loadable: %v", err)
	}
	return an
}

// contractOf 取 operation 的 contract 事实。
func contractOf(an *frontend.Analysis, method, path string) *facts.Fact {
	for _, f := range an.Facts {
		if f.ID == "contract:"+facts.OpKey(method, path) {
			return f
		}
	}
	return nil
}

// TestLLMWrapperSummaryAccepted L2：静态识别不出的 Wrap 经 LLM 摘要 + 复核后成为包装器，Profile 响应还原为
// WrapEnvelope{payload: User}，缺口闭合，置信度按 symbol 级封顶，带包装器证据。
func TestLLMWrapperSummaryAccepted(t *testing.T) {
	p := &fixedProvider{wrapper: `{"dataParam":1,"errParam":-1,"fields":[` +
		`{"key":"ok","source":"other","type":"boolean"},{"key":"payload","source":"data","type":"object"}]}`}
	an := analyzeGin(t, p)
	if an.Wrappers.Accepted != 1 || p.calls != 1 {
		t.Fatalf("wrapper stats = %+v, calls = %d", an.Wrappers, p.calls)
	}
	f := contractOf(an, "GET", "/api/v1/users/{id}/profile")
	cp, _ := f.Contract()
	if len(cp.Gaps) != 0 || f.Confidence != facts.VerificationCap(facts.VerifySymbol) {
		t.Fatalf("gaps=%v conf=%v", cp.Gaps, f.Confidence)
	}
	row := cp.Responses[0]
	if !strings.HasSuffix(row.SchemaType, ".User") || row.DataField != "payload" || !strings.HasSuffix(row.EnvelopeType, "WrapEnvelope") {
		t.Fatalf("row = %+v", row)
	}
	found := false
	for _, ev := range f.Evidence {
		found = found || strings.HasPrefix(ev.Quote, "llm:wrapper-summary Wrap")
	}
	if !found {
		t.Fatalf("missing wrapper evidence: %+v", f.Evidence)
	}
}

// TestLLMWrapperSummaryRejected 摘要里出现源码中不存在的键：整条拒绝，缺口保留。
func TestLLMWrapperSummaryRejected(t *testing.T) {
	p := &fixedProvider{wrapper: `{"dataParam":1,"errParam":-1,"fields":[{"key":"result","source":"data","type":"object"}]}`}
	an := analyzeGin(t, p)
	if an.Wrappers.Rejected != 1 || an.Wrappers.Accepted != 0 {
		t.Fatalf("wrapper stats = %+v", an.Wrappers)
	}
	cp, _ := contractOf(an, "GET", "/api/v1/users/{id}/profile").Contract()
	if len(cp.UnsummarizedWrappers) != 1 {
		t.Fatalf("gap must stay: %+v", cp.Gaps)
	}
}
