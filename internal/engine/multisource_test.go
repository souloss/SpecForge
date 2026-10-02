package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenAPIImportPreservesResponseDetails(t *testing.T) {
	openapiPath := filepath.Join(t.TempDir(), "input.yaml")
	input := `openapi: 3.1.0
info:
  title: External API
  version: "1"
paths:
  /external-only:
    get:
      responses:
        "200":
          description: Exact imported response description
          content:
            text/plain:
              schema:
                type: string
  /binary:
    get:
      responses:
        "200":
          description: Binary response
          content:
            application/octet-stream:
              schema:
                contentMediaType: application/octet-stream
`
	if err := os.WriteFile(openapiPath, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Run(context.Background(), Config{
		RepoDir:      filepath.Join("..", "..", "testdata", "gin-repo"),
		OutDir:       t.TempDir(),
		OpenAPIFiles: []string{openapiPath},
	})
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, operation := range result.Doc.Operations {
		if operation.Path != "/external-only" {
			continue
		}
		found = true
		if len(operation.Responses) != 1 {
			t.Fatalf("responses = %#v, want one response", operation.Responses)
		}
		response := operation.Responses[0]
		if response.ContentType != "text/plain" {
			t.Errorf("content type = %q, want text/plain", response.ContentType)
		}
		if !strings.Contains(response.Description, "Exact imported response description") {
			t.Errorf("description = %q, imported description missing", response.Description)
		}
		if !response.HasBody || response.SchemaName == "" {
			t.Errorf("response body/schema not preserved: %+v", response)
		}
	}
	if !found {
		t.Fatal("imported operation missing from compiled document")
	}
	contents, err := os.ReadFile(result.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "contentMediaType: application/octet-stream") {
		t.Fatalf("imported JSON Schema contentMediaType was lost:\n%s", contents)
	}
}
