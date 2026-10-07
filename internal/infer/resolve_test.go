package infer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// turnsProvider 首次带工具的调用报告轮数用尽，其后返回固定答案；记录每次请求。
type turnsProvider struct {
	reqs []Request // 收到的请求（按序）
}

func (p *turnsProvider) Name() string { return "turns" }
func (p *turnsProvider) Complete(_ context.Context, req Request) (Response, error) {
	p.reqs = append(p.reqs, req)
	if len(req.Tools) > 0 {
		return Response{}, ErrToolBudgetExhausted
	}
	return Response{Text: []byte(`{"errorSites":[{"site":"a@x.go:1","kind":"uncoded"}]}`)}, nil
}

// TestResolveGapsFallsBackWhenToolTurnsExhausted 工具循环用尽轮数时退回无工具单次调用并提示凭证据作答。
func TestResolveGapsFallsBackWhenToolTurnsExhausted(t *testing.T) {
	p := &turnsProvider{}
	res, err := ResolveGaps(context.Background(), p, "sys", GapTask{Method: "GET", Path: "/x"}, []ToolSpec{{Name: "read_source"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.reqs) != 2 || len(p.reqs[1].Tools) != 0 || !strings.HasSuffix(p.reqs[1].Prompt, prompt("no-tools.append.md")) {
		t.Fatalf("expected a tool-less retry with the fallback note, got %d requests", len(p.reqs))
	}
	if len(res.ErrorSites) != 1 || res.ErrorSites[0].Kind != SiteKindUncoded {
		t.Fatalf("unexpected resolution: %+v", res)
	}
}

func TestGapRequestCarriesUnifiedMetadata(t *testing.T) {
	task := GapTask{
		Source:        "static",
		Operation:     "POST /orders",
		AllowedFields: []string{"response.error_codes", "response.schema"},
		Method:        "POST",
		Path:          "/orders",
		Gaps:          []string{"response.error_codes"},
	}
	req, err := GapRequest("system", task, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got GapTask
	if err := json.Unmarshal([]byte(req.Prompt), &got); err != nil {
		t.Fatal(err)
	}
	if got.Source != task.Source || got.Operation != task.Operation || len(got.AllowedFields) != 2 {
		t.Fatalf("metadata was not preserved: %+v", got)
	}
}
