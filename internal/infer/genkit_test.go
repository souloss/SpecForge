package infer

import (
	"testing"
)

// TestNewProviderFromEnvOffline 无 API key 时返回 nil（离线模式），
// 保证接入 Genkit 后离线降级行为不变（回归守卫）。
func TestNewProviderFromEnvOffline(t *testing.T) {
	t.Setenv("ZAI_API_KEY", "")
	t.Setenv("SPECFORGE_LLM_API_KEY", "")
	if p := NewProviderFromEnv(); p != nil {
		t.Fatalf("NewProviderFromEnv() = %v, want nil (offline)", p)
	}
}

// TestFirstNonEmpty 边界。
func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "a", "b"); got != "a" {
		t.Fatalf("firstNonEmpty = %q, want %q", got, "a")
	}
	if got := firstNonEmpty(""); got != "" {
		t.Fatalf("firstNonEmpty(all empty) = %q, want empty", got)
	}
}
