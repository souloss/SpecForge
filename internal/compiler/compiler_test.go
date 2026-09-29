package compiler

import (
	"strings"
	"testing"

	"github.com/specforge/specforge/internal/schema"
)

func TestCanonicalShapeDedup(t *testing.T) {
	// 同形状不同名的类型 → 同一分组键（$ref 去重依据，设计文档 §8.3）
	a := &schema.Schema{Type: "object", Props: []schema.Prop{
		{Name: "x", Schema: &schema.Schema{Type: "string"}},
	}}
	b := &schema.Schema{Type: "object", Props: []schema.Prop{
		{Name: "x", Schema: &schema.Schema{Type: "string"}},
	}}
	if canonicalShape(a) != canonicalShape(b) {
		t.Error("same-shape schemas must hash identically")
	}
	c := &schema.Schema{Type: "object", Props: []schema.Prop{
		{Name: "y", Schema: &schema.Schema{Type: "string"}},
	}}
	if canonicalShape(a) == canonicalShape(c) {
		t.Error("different fields must hash differently")
	}
}

func TestPathLess(t *testing.T) {
	// 路径分段字典序（设计文档 §8.5 规则 2）
	if !pathLess("/a/b", "/a/c") {
		t.Error("/a/b < /a/c")
	}
	if !pathLess("/a", "/a/b") {
		t.Error("/a < /a/b (shorter prefix first)")
	}
	if pathLess("/a/c", "/a/b") {
		t.Error("/a/c must not be less than /a/b")
	}
}

func TestMethodOrder(t *testing.T) {
	// OpenAPI 规范推荐 method 顺序: get/put/post/delete/...
	if methodOrder("GET") >= methodOrder("PUT") {
		t.Error("get before put")
	}
	if methodOrder("PUT") >= methodOrder("POST") {
		t.Error("put before post")
	}
	if methodOrder("POST") >= methodOrder("DELETE") {
		t.Error("post before delete")
	}
}

func TestQuoteIfNeeded(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain", "plain"},
		{"", "''"},
		{"has: colon", "'has: colon'"},
		{"-leading", "'-leading'"},
		{"true", "'true'"},
		{"123", "'123'"},
	}
	for _, c := range cases {
		if got := quoteIfNeeded(c.in); got != c.want {
			t.Errorf("quoteIfNeeded(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSplitOpKey(t *testing.T) {
	m, p := splitOpKey("contract:op:GET:/health")
	if m != "GET" || p != "/health" {
		t.Errorf("got (%q, %q)", m, p)
	}
	m, p = splitOpKey("op:POST:/x")
	if m != "POST" || p != "/x" {
		t.Errorf("got (%q, %q)", m, p)
	}
}

func TestRenderDeterminism(t *testing.T) {
	doc := &Document{
		Title: "t", Version: "1.0.0",
		Operations: []Operation{{
			Method: "GET", Path: "/x", OperationID: "x",
			Responses: []ResponseOut{{
				Status: "200", Codes: []int{0, 40001},
				Description: "Success (code=0)", SchemaName: "X", HasBody: true,
			}},
		}},
		Schemas: []NamedSchema{{
			Name: "X", TypeID: "pkg.X",
			Schema: &schema.Schema{Type: "object", Props: []schema.Prop{
				{Name: "a", Schema: &schema.Schema{Type: "string"}},
			}, Required: []string{"a"}},
		}},
	}
	y1, _ := RenderYAML(doc)
	y2, _ := RenderYAML(doc)
	if string(y1) != string(y2) {
		t.Error("render must be deterministic")
	}
	if !strings.Contains(string(y1), "openapi: 3.1.0") {
		t.Error("must declare 3.1.0")
	}
}
