package infer

import (
	"testing"
)

// TestNewProviderFromEnvOffline 无认证环境变量时返回 nil（离线模式），
// 保证接入 Genkit 后离线降级行为不变（回归守卫）。
func TestNewProviderFromEnvOffline(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	if p := NewProviderFromEnv(); p != nil {
		t.Fatalf("NewProviderFromEnv() = %v, want nil (offline)", p)
	}
}

// TestNewProviderFromEnvModelDefault 默认模型名。
func TestNewProviderFromEnvModelDefault(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-test")
	t.Setenv("ANTHROPIC_MODEL", "")
	if p := NewProviderFromEnv(); p == nil {
		t.Fatalf("NewProviderFromEnv() = nil, want non-nil with auth token set")
	} else if p.Name() != "genkit:anthropic:deepseek-v4-pro-0813" {
		t.Fatalf("Name() = %q, want default model name", p.Name())
	}
}

// TestEnvOr 边界。
func TestEnvOr(t *testing.T) {
	t.Setenv("SPECFORGE_TEST_ENV", "present")
	if got := envOr("SPECFORGE_TEST_ENV", "def"); got != "present" {
		t.Fatalf("envOr = %q, want %q", got, "present")
	}
	if got := envOr("SPECFORGE_TEST_ENV_MISSING", "def"); got != "def" {
		t.Fatalf("envOr = %q, want default %q", got, "def")
	}
}
