package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/specforge/specforge/internal/contract"
)

func TestImportObservationsKeepsObservedEvidence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observations.jsonl")
	data := []byte(`{"method":"get","path":"/orders/{id}","status":200,"content_type":"application/json","trace_id":"t-1"}` + "\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	graph, err := ImportObservations(path)
	if err != nil {
		t.Fatal(err)
	}
	op := graph.Operations["GET /orders/{id}"]
	if op == nil || op.State != contract.OperationObservedOnly {
		t.Fatalf("operation = %+v", op)
	}
	candidates := op.Responses["200"]
	if len(candidates) != 1 || candidates[0].Status != contract.StatusObserved {
		t.Fatalf("responses = %+v", candidates)
	}
	ev := candidates[0].Evidence[0]
	if ev.Source != contract.SourceRuntime || ev.Location.TraceID != "t-1" {
		t.Fatalf("evidence = %+v", ev)
	}
}

func TestImportObservationsRejectsMalformedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "observations.jsonl")
	if err := os.WriteFile(path, []byte("not-json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportObservations(path); err == nil {
		t.Fatal("expected malformed observation error")
	}
}
