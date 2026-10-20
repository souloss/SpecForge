package verify

import (
	"strings"
	"testing"

	"github.com/specforge/specforge/internal/infer"
	"github.com/specforge/specforge/internal/schema"
)

// TestShapePassesConstraints 约束透传：enum/pattern/format/nullable 与数值、长度边界落到 schema，
// 且 enum 值与 pattern 字面量必须在源码里能找到（防幻觉）。
func TestShapePassesConstraints(t *testing.T) {
	text := `class R: status: Literal["pending","paid"] ; price = Field(ge=0, le=100) ; created_at: datetime`
	tokens := Tokens(text)
	node := &infer.ShapeNode{
		Type: "object",
		Properties: []infer.ShapeProp{
			{Name: "status", Shape: &infer.ShapeNode{Type: "string", Enum: []string{"pending", "paid"}}},
			{Name: "price", Shape: &infer.ShapeNode{Type: "number", Min: f64(0), Max: f64(100)}},
			{Name: "created_at", Shape: &infer.ShapeNode{Type: "string", Format: "date-time", Nullable: true}},
		},
	}
	sc, _, err := Shape(node, text, tokens, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Props) != 3 {
		t.Fatalf("props = %d, want 3", len(sc.Props))
	}
	byName := map[string]*schema.Prop{}
	for i := range sc.Props {
		byName[sc.Props[i].Name] = &sc.Props[i]
	}
	if got := byName["status"].Schema.Enum; len(got) != 2 || got[0] != "pending" {
		t.Fatalf("status enum = %v", got)
	}
	if p := byName["price"].Schema; p.Min == nil || *p.Min != 0 || p.Max == nil || *p.Max != 100 {
		t.Fatalf("price min/max = %v/%v", p.Min, p.Max)
	}
	if c := byName["created_at"].Schema; c.Format != "date-time" || !c.Nullable {
		t.Fatalf("created_at format/nullable = %q/%v", c.Format, c.Nullable)
	}
}

// TestShapeRejectsHallucinatedEnum 枚举值不在源码里：整棵拒绝。
func TestShapeRejectsHallucinatedEnum(t *testing.T) {
	text := `class R: status: str`
	node := &infer.ShapeNode{Type: "object", Properties: []infer.ShapeProp{
		{Name: "status", Shape: &infer.ShapeNode{Type: "string", Enum: []string{"ghost"}}},
	}}
	if _, _, err := Shape(node, text, Tokens(text), nil); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want rejection of hallucinated enum, got %v", err)
	}
}

// TestShapeResolvesSerializeAlias 模型报 Go 字段名（PascalCase）而源码 json tag 是 camelCase 时，
// 经 alias 映射核对并把产物字段名还原为序列化名（json tag）。
func TestShapeResolvesSerializeAlias(t *testing.T) {
	text := "type R struct { Data string `json:\"data\"`; Id int `json:\"id\"` }"
	node := &infer.ShapeNode{Type: "object", Properties: []infer.ShapeProp{
		{Name: "Data", Shape: &infer.ShapeNode{Type: "string"}},
		{Name: "Id", Shape: &infer.ShapeNode{Type: "integer", Format: "int64"}},
	}}
	alias := map[string]string{"Data": "data", "Id": "id"}
	sc, _, err := Shape(node, text, Tokens(text), alias)
	if err != nil {
		t.Fatalf("alias should let PascalCase field pass: %v", err)
	}
	names := map[string]bool{}
	for _, p := range sc.Props {
		names[p.Name] = true
	}
	if !names["data"] || !names["id"] || names["Data"] || names["Id"] {
		t.Fatalf("fields should be renamed to json tags, got %+v", names)
	}
}

// TestShapeDropsUnverifiedPattern 正则字面量不在源码里（如 Pydantic 约束写在 handler 切片外的模型类）：
// 丢弃该 pattern、保留字段与类型，而非整棵拒绝——避免因核对文本不含模型类而误拒正确形状。
func TestShapeDropsUnverifiedPattern(t *testing.T) {
	text := `class R: name: str`
	node := &infer.ShapeNode{Type: "object", Properties: []infer.ShapeProp{
		{Name: "name", Shape: &infer.ShapeNode{Type: "string", Pattern: "^[A-Z]+$"}},
	}}
	sc, _, err := Shape(node, text, Tokens(text), nil)
	if err != nil {
		t.Fatalf("unverified pattern should be dropped, not rejected: %v", err)
	}
	for _, p := range sc.Props {
		if p.Name == "name" && p.Schema.Pattern != "" {
			t.Fatalf("pattern should be cleared when not in source, got %q", p.Schema.Pattern)
		}
	}
}

// f64 float64 常量指针。
func f64(v float64) *float64 { return &v }
