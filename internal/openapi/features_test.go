package openapi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyOverlayReloadsAndValidatesResult(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.yaml")
	overlay := filepath.Join(dir, "overlay.yaml")
	if err := os.WriteFile(base, []byte(validDocument), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlay, []byte(`overlay: 1.0.0
info:
  title: test overlay
actions:
  - target: $.info
    update:
      title: overlaid
`), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyOverlay(base, overlay)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Document.Close()
	if len(result.Bytes) == 0 || len(result.Document.Validate()) != 0 {
		t.Fatalf("overlay result was not valid: %+v", result)
	}
}

func TestCompareDocumentsIsSeparateFromContractGraphDiff(t *testing.T) {
	dir := t.TempDir()
	left := filepath.Join(dir, "left.yaml")
	right := filepath.Join(dir, "right.yaml")
	if err := os.WriteFile(left, []byte(validDocument), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := []byte(validDocument + "\n")
	if err := os.WriteFile(right, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	diff, err := CompareDocuments(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if diff == nil {
		t.Fatal("nil document diff")
	}
}

func TestLoadArazzoKeepsWorkflowExecutionStateSeparate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.yaml")
	data := []byte(`arazzo: 1.0.1
info:
  title: workflow
  version: 1.0.0
sourceDescriptions:
  - name: api
    url: https://example.test/openapi.yaml
    type: openapi
workflows:
  - workflowId: fetch
    steps:
      - stepId: get
        operationId: getPet
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := LoadArazzo(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics) != 0 || len(result.Graph.Workflows) != 1 || result.Graph.Workflows[0].Steps[0].Executed {
		t.Fatalf("Arazzo result = %+v", result)
	}
}
