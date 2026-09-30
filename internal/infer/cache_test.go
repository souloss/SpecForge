package infer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// countingProvider 记录调用次数；Tools 非空时调用一次工具（模拟档位3 读代码）。
type countingProvider struct {
	calls int
	out   string
}

func (c *countingProvider) Name() string { return "counting" }
func (c *countingProvider) Complete(ctx context.Context, req Request) (Response, error) {
	c.calls++
	for _, t := range req.Tools {
		if _, err := dispatchTool(ctx, t.Name, map[string]any{"id": "x"}); err != nil {
			return Response{}, err
		}
	}
	return Response{Text: []byte(c.out), Usage: Usage{Calls: 1, InputTokens: 100, OutputTokens: 20}}, nil
}

// TestCachedProviderHitMiss 同请求第二次命中且计入节省；请求内容变化即换键。
func TestCachedProviderHitMiss(t *testing.T) {
	inner := &countingProvider{out: "{}"}
	p := NewCachedProvider(inner, t.TempDir()).(*CachedProvider)
	ctx := context.Background()
	req := Request{System: "s", Prompt: "p"}
	for i := 0; i < 2; i++ {
		if _, err := p.Complete(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if inner.calls != 1 {
		t.Fatalf("inner calls = %d, want 1", inner.calls)
	}
	if st := p.CacheStats(); st.Hits != 1 || st.Misses != 1 || st.SavedTokens != 120 {
		t.Fatalf("stats = %+v", st)
	}
	req.Prompt = "p2"
	p.Complete(ctx, req)
	if inner.calls != 2 {
		t.Fatalf("changed prompt must miss, calls = %d", inner.calls)
	}
}

// TestCachedProviderReadSetValidation 工具读集：被读代码不变 → 命中；变了 → 判陈旧重算（Salsa 式校验）。
func TestCachedProviderReadSetValidation(t *testing.T) {
	inner := &countingProvider{out: "{}"}
	p := NewCachedProvider(inner, t.TempDir()).(*CachedProvider)
	code := "v1"
	tool := ToolSpec{Name: "read_symbol", Call: func(context.Context, map[string]any) (string, error) { return code, nil }}
	req := Request{System: "s", Prompt: "p", Tools: []ToolSpec{tool}, MaxTurns: 3}
	ctx := context.Background()
	p.Complete(ctx, req)
	p.Complete(ctx, req)
	if inner.calls != 1 {
		t.Fatalf("unchanged read set should hit, calls = %d", inner.calls)
	}
	code = "v2" // 模型读过的代码变了
	p.Complete(ctx, req)
	if inner.calls != 2 || p.CacheStats().Stale != 1 {
		t.Fatalf("changed read set should be stale: calls=%d stats=%+v", inner.calls, p.CacheStats())
	}
}

// TestNewCachedProviderDisabled 目录为空或 provider 为 nil 时不包装。
func TestNewCachedProviderDisabled(t *testing.T) {
	inner := &countingProvider{}
	if NewCachedProvider(inner, "") != Provider(inner) {
		t.Fatal("empty dir should return inner provider")
	}
	if NewCachedProvider(nil, "x") != nil {
		t.Fatal("nil provider should stay nil")
	}
}

// TestExtractJSON 去围栏与前后说明文字。
func TestExtractJSON(t *testing.T) {
	for in, want := range map[string]string{
		"```json\n{\"a\":1}\n```":       `{"a":1}`,
		"Here you go: {\"a\":1} thanks": `{"a":1}`,
		`{"a":{"b":2}}`:                 `{"a":{"b":2}}`,
	} {
		if got := string(extractJSON([]byte(in))); got != want {
			t.Errorf("extractJSON(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSortCatalogDeterministic 目录顺序与输入顺序无关（任务 JSON 稳定，缓存键稳定）。
func TestSortCatalogDeterministic(t *testing.T) {
	a := SortCatalog([]ErrorCodeItem{{Symbol: "p.B", Code: 2}, {Symbol: "p.A", Code: 1}})
	if a[0].Symbol != "p.A" || a[1].Symbol != "p.B" {
		t.Fatalf("catalog must be sorted by symbol: %+v", a)
	}
}

// TestResolveGapsRepairsInvalidJSON 首次输出不合法时带错误重试一次。
func TestResolveGapsRepairsInvalidJSON(t *testing.T) {
	p := &seqProvider{outs: []string{`{errorCodes: nope}`, `{"errorCodes":[{"codeRef":"p.A"}]}`}}
	res, err := ResolveGaps(context.Background(), p, "s", GapTask{}, nil)
	if err != nil || len(res.ErrorCodes) != 1 || p.i != 2 || !strings.Contains(p.lastPrompt, "could not be parsed") {
		t.Fatalf("res=%+v err=%v calls=%d", res, err, p.i)
	}
}

// seqProvider 依次返回预设输出，记录最后一次 prompt。
type seqProvider struct {
	outs       []string
	i          int
	lastPrompt string
}

func (s *seqProvider) Name() string { return "seq" }
func (s *seqProvider) Complete(_ context.Context, req Request) (Response, error) {
	s.lastPrompt = req.Prompt
	out := s.outs[s.i]
	s.i++
	return Response{Text: []byte(out)}, nil
}

// TestResolveGapsFallsBackWithoutTools 不支持工具的 provider 自动退回档位2。
func TestResolveGapsFallsBackWithoutTools(t *testing.T) {
	p := &noToolsProvider{}
	res, err := ResolveGaps(context.Background(), p, "s", GapTask{}, []ToolSpec{{Name: "x"}})
	if err != nil || res == nil || p.calls != 2 {
		t.Fatalf("res=%v err=%v calls=%d", res, err, p.calls)
	}
}

// noToolsProvider 带工具时返回 ErrToolsUnsupported。
type noToolsProvider struct{ calls int }

func (n *noToolsProvider) Name() string { return "no-tools" }
func (n *noToolsProvider) Complete(_ context.Context, req Request) (Response, error) {
	n.calls++
	if len(req.Tools) > 0 {
		return Response{}, ErrToolsUnsupported
	}
	return Response{Text: []byte(`{"errorCodes":[]}`)}, nil
}

// TestResolveGapsOffline 无 provider 时返回 ErrNoProvider。
func TestResolveGapsOffline(t *testing.T) {
	if _, err := ResolveGaps(context.Background(), nil, "", GapTask{}, nil); err != ErrNoProvider {
		t.Fatalf("want ErrNoProvider, got %v", err)
	}
}

// TestCachedProviderRemembersExhaustedToolLoop 工具预算耗尽被负缓存：重跑直接报耗尽、不再调用内层，且不算「已缓存」。
func TestCachedProviderRemembersExhaustedToolLoop(t *testing.T) {
	inner := &turnsProvider{}
	p := NewCachedProvider(inner, t.TempDir()).(*CachedProvider)
	ctx := context.Background()
	req := Request{System: "s", Prompt: "p", Tools: []ToolSpec{{Name: "read_source"}}, MaxTurns: 3}
	for i := 0; i < 2; i++ {
		if _, err := p.Complete(ctx, req); !errors.Is(err, ErrToolBudgetExhausted) {
			t.Fatalf("run %d: err = %v, want ErrToolBudgetExhausted", i, err)
		}
	}
	if len(inner.reqs) != 1 {
		t.Fatalf("inner calls = %d, want 1", len(inner.reqs))
	}
	if p.Cached(ctx, req) {
		t.Fatal("an exhausted entry must not count as cached")
	}
}
