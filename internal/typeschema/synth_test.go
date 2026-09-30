package typeschema

import (
	"testing"

	"github.com/specforge/specforge/internal/codegraph"
)

// TestSynthesizeEmptyStruct 匿名空结构体（如上游响应 `Result struct{}`）恒序列化为 {}：
// 合成为空 object，不是 unknown（不产生形状缺口）；其它未登记类型仍显式 unknown。
func TestSynthesizeEmptyStruct(t *testing.T) {
	s := New(codegraph.NewGraph())
	if sc := s.Synthesize(emptyStructID); sc.Unknown || sc.Type != "object" || len(sc.Props) != 0 {
		t.Fatalf("struct{} schema = %+v", sc)
	}
	if sc := s.Synthesize("m/x.Missing"); !sc.Unknown {
		t.Fatalf("missing type must stay unknown: %+v", sc)
	}
}
