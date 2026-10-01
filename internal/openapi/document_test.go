package openapi

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const validDocument = `openapi: 3.1.0
info:
  title: test
  version: 1.0.0
paths:
  /pets/{id}:
    get:
      operationId: getPet
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: string
        - name: q
          in: query
          required: true
          schema:
            type: string
      responses:
        '200':
          description: ok
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Pet'
components:
  schemas:
    Pet:
      type: object
      required: [name]
      properties:
        name:
          type: string
`

func TestLoadValidateSupports31AndHTTPValidation(t *testing.T) {
	doc, err := LoadBytes([]byte(validDocument), "spec.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	if diagnostics := doc.Validate(); len(diagnostics) != 0 {
		t.Fatalf("valid document diagnostics: %+v", diagnostics)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.test/pets/42?q=cat", nil)
	if result := doc.ValidateRequest(req); result.Status != "valid" {
		t.Fatalf("request status = %q, diagnostics = %+v", result.Status, result.Diagnostics)
	}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(`{"name":"cat"}`))}
	if result := doc.ValidateResponse(req, resp); result.Status != "valid" {
		t.Fatalf("response status = %q, diagnostics = %+v", result.Status, result.Diagnostics)
	}

	badReq := httptest.NewRequest(http.MethodGet, "http://example.test/pets/42", nil)
	if result := doc.ValidateRequest(badReq); result.Status != "request_invalid" {
		t.Fatalf("bad request status = %q, diagnostics = %+v", result.Status, result.Diagnostics)
	}
	missing := httptest.NewRequest(http.MethodGet, "http://example.test/unknown", nil)
	if result := doc.ValidateRequest(missing); result.Status != "operation_not_found" {
		t.Fatalf("missing operation status = %q, diagnostics = %+v", result.Status, result.Diagnostics)
	}
}

func TestLoadResolvesExternalReference(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.yaml"), []byte("type: string\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := `openapi: 3.0.3
info: {title: external, version: '1'}
paths:
  /x:
    get:
      responses:
        '200':
          description: ok
          content:
            application/json:
              schema:
                $ref: './schema.yaml'
`
	path := filepath.Join(dir, "openapi.yaml")
	if err := os.WriteFile(path, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	if diagnostics := doc.Validate(); len(diagnostics) != 0 {
		t.Fatalf("external ref diagnostics: %+v", diagnostics)
	}
}

func TestLoadRejectsBrokenReference(t *testing.T) {
	spec := []byte(`openapi: 3.1.0
info: {title: broken, version: 1}
paths:
  /x:
    get:
      responses:
        '200':
          description: ok
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Missing'}
`)
	if _, err := LoadBytes(spec, "broken.yaml"); err == nil {
		t.Fatal("expected broken reference error")
	}
}

func TestLoadRejectsInvalidDocument(t *testing.T) {
	doc, err := LoadBytes([]byte("openapi: 3.1.0\n"), "invalid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	if diagnostics := doc.Validate(); len(diagnostics) == 0 {
		t.Fatal("expected invalid document diagnostics")
	}
}
