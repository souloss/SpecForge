package infer

import (
	"context"
	"testing"
)

// mockProvider 返回预设 JSON 的 Provider（learn 测试用）。
type learnMockProvider struct{ out string }

func (m *learnMockProvider) Name() string { return "learn-mock" }
func (m *learnMockProvider) Complete(_ context.Context, _ Request) (Response, error) {
	return Response{Text: []byte(m.out)}, nil
}

// TestLearnProfileOffline 无 provider 返回 ErrNoProvider。
func TestLearnProfileOffline(t *testing.T) {
	if _, err := LearnProfile(context.Background(), nil, nil); err != ErrNoProvider {
		t.Fatalf("want ErrNoProvider, got %v", err)
	}
}

// TestLearnProfileParses 合法输出能解析出画像候选。
func TestLearnProfileParses(t *testing.T) {
	p := &learnMockProvider{out: `{"framework":"gofiber/v2","responseSinks":[{"symbol":"code.WriteResponse","signature":"(c *fiber.Ctx, err error, data any) error","dataSlot":2,"errSlot":1,"status":200}],"authMiddleware":{"auth.GetUserRelation":{"header":"X-User-Token","scheme":"apiKey","required":true}}}`}
	cand, err := LearnProfile(context.Background(), p, []SampleOp{{Method: "POST", Path: "/ipo/v1/OrderCheck"}})
	if err != nil {
		t.Fatalf("LearnProfile: %v", err)
	}
	if cand.Framework != "gofiber/v2" {
		t.Fatalf("framework = %q", cand.Framework)
	}
	if len(cand.ResponseSinks) != 1 || cand.ResponseSinks[0].Symbol != "code.WriteResponse" {
		t.Fatalf("responseSinks = %+v", cand.ResponseSinks)
	}
	if cand.AuthMiddleware["auth.GetUserRelation"].Header != "X-User-Token" {
		t.Fatalf("authMiddleware = %+v", cand.AuthMiddleware)
	}
}

// TestLearnProfileRejectsInvalidJSON 非法 JSON 返回错误。
func TestLearnProfileRejectsInvalidJSON(t *testing.T) {
	p := &learnMockProvider{out: `not-json`}
	if _, err := LearnProfile(context.Background(), p, nil); err == nil {
		t.Fatal("want error for invalid JSON, got nil")
	}
}
