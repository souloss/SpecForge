package slicing

import (
	"testing"

	"github.com/specforge/specforge/internal/adapter"
	"github.com/specforge/specforge/internal/profile"
)

func TestSummarizeWrapperIgnoresWriterWithoutBodySlot(t *testing.T) {
	_, graph := buildTestSlicer(t, `package svc
func response(data interface{}) {}
func handler(value interface{}) { response(value) }
`)
	writer := adapter.Writer{Symbol: "m/svc.response", BodyArg: profile.NoSlot}
	_, _, ok := summarizeWrapper(graph, "m/svc.handler", map[string]adapter.Writer{writer.Symbol: writer})
	if ok {
		t.Fatal("writer without a body argument must not form a wrapper")
	}
}
