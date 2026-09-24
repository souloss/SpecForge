package adapter

import (
	"testing"
)

func TestNormalizePath(t *testing.T) {
	cases := []struct {
		in       string
		want     string
		wildcard bool
	}{
		{"/users/:id", "/users/{id}", false},
		{"/users/:id/items/:itemId", "/users/{id}/items/{itemId}", false},
		{"/ipo/v*", "/ipo/v*", true},
		{"/ipo/v*/OrderCheck", "/ipo/v*/OrderCheck", true},
		{"/health", "/health", false},
		{"/a/:x/*", "/a/{x}/*", true},
	}
	for _, c := range cases {
		got, wc := normalizePath(c.in)
		if got != c.want || wc != c.wildcard {
			t.Errorf("normalizePath(%q) = (%q, %v), want (%q, %v)", c.in, got, wc, c.want, c.wildcard)
		}
	}
}

func TestStringArg(t *testing.T) {
	// 非 BasicLit → false（动态路径判定依赖）
	if _, ok := stringArg(nil); ok {
		t.Error("nil should not be a string arg")
	}
}

func TestDetectFramework(t *testing.T) {
	if got := DetectFramework([]string{"github.com/gofiber/fiber/v2"}); got != "gofiber/v2" {
		t.Errorf("fiber detect = %q", got)
	}
	if got := DetectFramework([]string{"github.com/gin-gonic/gin"}); got != "gin" {
		t.Errorf("gin detect = %q", got)
	}
	if got := DetectFramework([]string{"github.com/other/lib"}); got != "" {
		t.Errorf("unknown should be empty, got %q", got)
	}
}
