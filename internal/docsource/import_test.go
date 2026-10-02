package docsource

import (
	"github.com/specforge/specforge/internal/contract"
	"os"
	"path/filepath"
	"testing"
)

func TestImportCreatesDeclaredCandidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.yaml")
	data := []byte("operations:\n  - method: get\n    path: /pets/{id}\n    responses:\n      '200':\n        description: ok\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	graph, err := Import(path)
	if err != nil {
		t.Fatal(err)
	}
	op := graph.Operations["GET /pets/{id}"]
	if op == nil || op.State != contract.OperationDocumentOnly || op.Confidence != .7 {
		t.Fatalf("operation = %+v", op)
	}
	if op.Evidence[0].Source != contract.SourceDocumentation {
		t.Fatalf("evidence = %+v", op.Evidence)
	}
}

func TestImportRejectsMissingResponses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.yaml")
	if err := os.WriteFile(path, []byte("operations:\n  - method: get\n    path: /pets\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(path); err == nil {
		t.Fatal("expected missing responses error")
	}
}
