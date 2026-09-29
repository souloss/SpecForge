package golang

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/infer"
	"github.com/specforge/specforge/internal/loader"
	"github.com/specforge/specforge/internal/profile"
)

// fixtureProgram 临时模块的程序视图（含 m/svc.NewError）。
func fixtureProgram(t *testing.T) *Program {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n\ngo 1.21\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "svc"), 0o755)
	os.WriteFile(filepath.Join(dir, "svc", "svc.go"), []byte("package svc\n\nimport \"errors\"\n\nfunc NewError(code int) error { return errors.New(\"x\") }\n"), 0o644)
	l, err := loader.LoadRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	g, err := codegraph.Build(l.Pkgs, l.Fset)
	if err != nil {
		t.Fatal(err)
	}
	g.SetRoot(l.Root)
	return NewProgram(g)
}

// TestMergeProfileCandidateFillsMissing 画像候选填补缺失（framework / sinks）。
func TestMergeProfileCandidateFillsMissing(t *testing.T) {
	cand := &infer.ProfileCandidate{
		Framework:     "gofiber/v2",
		ResponseSinks: []infer.SinkCandidate{{Symbol: "code.WriteResponse", DataSlot: 2, ErrSlot: 1, Status: 200}},
	}
	merged := mergeProfileCandidate(profile.Default(), cand)
	if merged.Framework != "gofiber/v2" || len(merged.ResponseSinks) != 1 {
		t.Fatalf("merged = %+v", merged)
	}
}

// TestMergeProfileCandidateDoesNotOverride 画像候选不覆盖既有约定。
func TestMergeProfileCandidateDoesNotOverride(t *testing.T) {
	prof := &profile.Profile{
		Framework:      "gofiber/v2",
		ResponseSinks:  []profile.SinkPattern{{Symbol: "existing.Sink"}},
		AuthMiddleware: map[string]profile.SecurityMapping{},
	}
	merged := mergeProfileCandidate(prof, &infer.ProfileCandidate{Framework: "gin", ResponseSinks: []infer.SinkCandidate{{Symbol: "llm.Sink"}}})
	if merged.Framework != "gofiber/v2" || merged.ResponseSinks[0].Symbol != "existing.Sink" {
		t.Fatalf("merged = %+v", merged)
	}
}

// TestVerifyProfileCandidateDropsUnknownSinks 代码图中不存在的汇聚点符号被丢弃。
func TestVerifyProfileCandidateDropsUnknownSinks(t *testing.T) {
	cand := verifyProfileCandidate(fixtureProgram(t), &infer.ProfileCandidate{ResponseSinks: []infer.SinkCandidate{
		{Symbol: "m/svc.NewError"}, {Symbol: "code.Imaginary"},
	}})
	if len(cand.ResponseSinks) != 1 || cand.ResponseSinks[0].Symbol != "m/svc.NewError" {
		t.Fatalf("sinks = %+v", cand.ResponseSinks)
	}
}

// TestUniqueFieldTypesDropsConflicts 同名字段类型冲突时不采信代码图（结果与 map 遍历序无关）。
func TestUniqueFieldTypesDropsConflicts(t *testing.T) {
	g := &codegraph.Graph{Types: map[string]*codegraph.TypeInfo{
		"a.T": {IsStruct: true, Fields: []codegraph.Field{{JSONName: "id", TypeStr: "string"}, {JSONName: "n", TypeStr: "int"}}},
		"b.T": {IsStruct: true, Fields: []codegraph.Field{{JSONName: "id", TypeStr: "int64"}, {JSONName: "n", TypeStr: "int32"}}},
	}}
	got := uniqueFieldTypes(g)
	if _, ok := got["id"]; ok || got["n"] != "integer" {
		t.Fatalf("got %v", got)
	}
}

// TestFrameworkCheckRequireForms go.mod 单行与块状 require 都能识别框架。
func TestFrameworkCheckRequireForms(t *testing.T) {
	single := []byte("module x\n\ngo 1.22\n\nrequire github.com/gin-gonic/gin v1.10.0\n")
	block := []byte("module x\n\ngo 1.22\n\nrequire (\n\tgithub.com/gofiber/fiber/v2 v2.52.5\n)\n")
	if c := frameworkCheck(single); !c.OK || c.Detail != "gin" {
		t.Fatalf("single-line require: %+v", c)
	}
	if c := frameworkCheck(block); !c.OK || c.Detail != "gofiber/v2" {
		t.Fatalf("block require: %+v", c)
	}
}
