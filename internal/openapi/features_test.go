package openapi

import (
	"os"
	"path/filepath"
	"strings"
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

func TestCompareDocumentsUsesStableChangeKinds(t *testing.T) {
	dir := t.TempDir()
	left := filepath.Join(dir, "left.yaml")
	right := filepath.Join(dir, "right.yaml")
	if err := os.WriteFile(left, []byte(validDocument), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := []byte(`openapi: 3.1.0
info: {title: changed, version: '1'}
paths: {}
`)
	if err := os.WriteFile(right, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	diff, err := CompareDocuments(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Changes) == 0 || diff.Changes[0].Kind == "" || strings.HasPrefix(diff.Changes[0].Kind, "change-") {
		t.Fatalf("change kind was not normalized: %+v", diff.Changes)
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
        operationPath: api.getPet
        parameters:
          - name: id
            in: query
            value: 42
        successCriteria:
          - condition: $statusCode == 200
`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := LoadArazzo(path)
	if err != nil {
		t.Fatal(err)
	}
	step := result.Graph.Workflows[0].Steps[0]
	if len(result.Diagnostics) != 0 || len(result.Graph.Workflows) != 1 || step.Executed || step.OperationPath != "api.getPet" || len(step.Parameters) != 1 || step.Parameters[0].Name != "id" || step.Parameters[0].Value != 42 {
		t.Fatalf("Arazzo result = %+v", result)
	}
}

func TestImportBuildsContractGraphWithRefsAndResponses(t *testing.T) {
	graph, err := ImportBytes([]byte(validDocument), "openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	op, ok := graph.Operations["GET /pets/{id}"]
	if !ok {
		t.Fatalf("operation not imported: %+v", graph.Operations)
	}
	if op.OperationID != "getPet" || len(op.Parameters["id"]) != 1 || len(op.Responses["200"]) != 1 {
		t.Fatalf("unexpected imported operation: %+v", op)
	}
	if graph.Schemas["Pet"] == nil || graph.Schemas["Pet"].Properties["name"] == nil {
		t.Fatalf("schema was not imported: %+v", graph.Schemas)
	}
	if op.Responses["200"][0].Value.Schema != "Pet" {
		t.Fatalf("response reference was not retained: %+v", op.Responses["200"])
	}
}

func TestImportRejectsInvalidOpenAPIDocument(t *testing.T) {
	if _, err := ImportBytes([]byte("openapi: 3.1.0\n"), "invalid.yaml"); err == nil || !strings.Contains(err.Error(), "failed validation") {
		t.Fatalf("ImportBytes error = %v, want validation failure", err)
	}
}

func TestImportPreservesJSONSchemaContentKeywords(t *testing.T) {
	input := `openapi: 3.1.0
info: {title: binary, version: '1'}
paths:
  /download:
    get:
      responses:
        '200':
          description: binary body
          content:
            application/octet-stream:
              schema:
                contentEncoding: base64
                contentMediaType: application/octet-stream
`
	graph, err := ImportBytes([]byte(input), "binary.yaml")
	if err != nil {
		t.Fatal(err)
	}
	response := graph.Operations["GET /download"].Responses["200"][0].Value
	schema := graph.Schemas[response.Schema]
	if schema == nil || schema.ContentEncoding != "base64" || schema.ContentMediaType != "application/octet-stream" {
		t.Fatalf("JSON Schema content keywords were lost: response=%+v schema=%+v", response, schema)
	}
}
