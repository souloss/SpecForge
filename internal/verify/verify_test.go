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
	sc, _, err := Shape(node, text, tokens)
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
	if _, _, err := Shape(node, text, Tokens(text)); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want rejection of hallucinated enum, got %v", err)
	}
}

// TestShapeRejectsHallucinatedPattern 正则字面量不在源码里：整棵拒绝。
func TestShapeRejectsHallucinatedPattern(t *testing.T) {
	text := `class R: name: str`
	node := &infer.ShapeNode{Type: "object", Properties: []infer.ShapeProp{
		{Name: "name", Shape: &infer.ShapeNode{Type: "string", Pattern: "^[A-Z]+$"}},
	}}
	if _, _, err := Shape(node, text, Tokens(text)); err == nil {
		t.Fatal("want rejection of hallucinated pattern")
	}
}

// f64 float64 常量指针。
func f64(v float64) *float64 { return &v }
