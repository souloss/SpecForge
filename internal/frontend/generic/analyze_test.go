package generic

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/specforge/specforge/internal/infer"
)

// TestNormalizePath 各框架路径参数写法归一为 OpenAPI {name}。
func TestNormalizePath(t *testing.T) {
	cases := map[string]struct {
		path   string
		params []string
	}{
		"/users/:id":                 {"/users/{id}", []string{"id"}},
		"/users/<int:uid>/posts/<p>": {"/users/{uid}/posts/{p}", []string{"uid", "p"}},
		"/items/{id:[0-9]+}/":        {"/items/{id}", []string{"id"}},
		"/":                          {"/", nil},
	}
	for in, want := range cases {
		got, params := normalizePath(in)
		if got != want.path || !reflect.DeepEqual(params, want.params) {
			t.Errorf("normalizePath(%q) = %q %v, want %q %v", in, got, params, want.path, want.params)
		}
	}
}

func TestVerifyRouteAllowsTraceButNotConnect(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "routes.js")
	if err := os.WriteFile(file, []byte("router.trace('/events', traceEvents)\nfunction traceEvents() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prog, err := NewProgram(root)
	if err != nil {
		t.Fatal(err)
	}
	trace, err := verifyRoute(prog, file, infer.DiscoveredRoute{Method: "TRACE", Path: "/events", Handler: "traceEvents", Line: 1})
	if err != nil || trace.method != "TRACE" {
		t.Fatalf("TRACE route = %+v, error = %v", trace, err)
	}
	if _, err := verifyRoute(prog, file, infer.DiscoveredRoute{Method: "CONNECT", Path: "/events", Handler: "traceEvents", Line: 1}); err == nil {
		t.Fatal("CONNECT cannot be represented as an OpenAPI Path Item operation")
	}
}

// TestPathLiteralIn 注册行上的字面量须等于所报路径或按段边界为其后缀。
func TestPathLiteralIn(t *testing.T) {
	window := `router.get('/users/:id', getUser)`
	if lit, ok := pathLiteralIn(window, "/api/users/:id"); !ok || lit != "/users/:id" {
		t.Fatalf("suffix literal not found: %q %v", lit, ok)
	}
	if _, ok := pathLiteralIn(window, "/api/admin"); ok {
		t.Fatal("unrelated path must not match")
	}
	if _, ok := pathLiteralIn(`@GetMapping("ers/:id")`, "/users/:id"); ok {
		t.Fatal("suffix must fall on a segment boundary")
	}
}
