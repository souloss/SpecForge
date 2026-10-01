// Package openapi contains the libopenapi boundary. Third-party document types
// remain private to this package.
package openapi

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
	validatorerrors "github.com/pb33f/libopenapi-validator/errors"
	"github.com/pb33f/libopenapi/datamodel"

	runtimevalidation "github.com/specforge/specforge/internal/runtime"
)

// Diagnostic is a stable SpecForge diagnostic for document validation.
type Diagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
}

// Document wraps the parsed and validated OpenAPI document.
type Document struct {
	document     libopenapi.Document
	validator    validator.Validator
	validateOnce sync.Once
	validation   []Diagnostic
}

var _ runtimevalidation.Validator = (*Document)(nil)

// Load reads and builds an OpenAPI 3 document, resolving local file references
// relative to the document. Remote references stay disabled.
func Load(path string) (*Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read OpenAPI document %q: %w", path, err)
	}
	return LoadBytes(data, path)
}

// LoadBytes parses and builds an OpenAPI 3 document. sourcePath supplies the
// base directory for local references and is used for diagnostics.
func LoadBytes(data []byte, sourcePath string) (*Document, error) {
	config := datamodel.NewDocumentConfiguration()
	if sourcePath != "" {
		abs, err := filepath.Abs(sourcePath)
		if err != nil {
			return nil, fmt.Errorf("resolve OpenAPI source path %q: %w", sourcePath, err)
		}
		config.BasePath = filepath.Dir(abs)
		config.SpecFilePath = abs
	}
	config.AllowRemoteReferences = false

	doc, err := libopenapi.NewDocumentWithConfiguration(data, config)
	if err != nil {
		return nil, fmt.Errorf("parse OpenAPI document %q: %w", sourcePath, err)
	}
	model, err := doc.BuildV3Model()
	if err != nil {
		doc.Release()
		return nil, fmt.Errorf("build OpenAPI model %q: %w", sourcePath, err)
	}
	if model == nil {
		doc.Release()
		return nil, fmt.Errorf("build OpenAPI model %q: no OpenAPI 3 model", sourcePath)
	}

	requestValidator, errs := validator.NewValidator(doc)
	if len(errs) != 0 {
		doc.Release()
		return nil, fmt.Errorf("initialize OpenAPI validator %q: %w", sourcePath, errors.Join(errs...))
	}
	return &Document{document: doc, validator: requestValidator}, nil
}

// Validate checks the document against the OpenAPI specification.
func (d *Document) Validate() []Diagnostic {
	if d == nil || d.validator == nil {
		return []Diagnostic{{Code: "document_unavailable", Message: "OpenAPI document is not loaded"}}
	}
	d.validateOnce.Do(func() {
		valid, errs := d.validator.ValidateDocument()
		d.validation = diagnosticsFromValidator(valid, errs, "document_invalid")
	})
	return append([]Diagnostic(nil), d.validation...)
}

// ValidateRequest checks one request and reports whether the operation exists.
func (d *Document) ValidateRequest(req *http.Request) runtimevalidation.ValidationResult {
	if d == nil || d.validator == nil || req == nil {
		return runtimevalidation.ValidationResult{Status: runtimevalidation.StatusInconclusive,
			Diagnostics: []runtimevalidation.Diagnostic{{Code: "request_unavailable", Message: "OpenAPI document or request is unavailable"}}}
	}
	valid, errs := d.validator.ValidateHttpRequestSync(req)
	status := runtimevalidation.StatusValid
	if !valid {
		status = runtimevalidation.StatusRequestInvalid
		for _, issue := range errs {
			if issue != nil && (issue.IsOperationMissingError() || issue.IsPathMissingError()) {
				status = runtimevalidation.StatusOperationMissing
				break
			}
		}
	}
	return runtimevalidation.ValidationResult{Status: status, Diagnostics: runtimeDiagnostics(valid, errs)}
}

// ValidateResponse checks one response against the operation selected by req.
func (d *Document) ValidateResponse(req *http.Request, resp *http.Response) runtimevalidation.ValidationResult {
	if d == nil || d.validator == nil || req == nil || resp == nil {
		return runtimevalidation.ValidationResult{Status: runtimevalidation.StatusInconclusive,
			Diagnostics: []runtimevalidation.Diagnostic{{Code: "response_unavailable", Message: "OpenAPI document, request, or response is unavailable"}}}
	}
	valid, errs := d.validator.ValidateHttpResponse(req, resp)
	status := runtimevalidation.StatusValid
	if !valid {
		status = runtimevalidation.StatusResponseInvalid
		for _, issue := range errs {
			if issue != nil && (issue.IsOperationMissingError() || issue.IsPathMissingError()) {
				status = runtimevalidation.StatusOperationMissing
				break
			}
		}
	}
	return runtimevalidation.ValidationResult{Status: status, Diagnostics: runtimeDiagnostics(valid, errs)}
}

// Close releases third-party document state and its indexes.
func (d *Document) Close() {
	if d != nil && d.document != nil {
		d.document.Release()
	}
}

func diagnosticsFromValidator(valid bool, errs []*validatorerrors.ValidationError, fallback string) []Diagnostic {
	if valid {
		return nil
	}
	out := make([]Diagnostic, 0, len(errs))
	for _, issue := range errs {
		if issue == nil {
			continue
		}
		code := issue.ValidationType
		if code == "" {
			code = fallback
		}
		message := strings.TrimSpace(issue.Message)
		if issue.Reason != "" {
			message = strings.TrimSpace(message + ": " + issue.Reason)
		}
		out = append(out, Diagnostic{Code: code, Message: message, Path: issue.SpecPath, Line: issue.SpecLine, Column: issue.SpecCol})
	}
	if len(out) == 0 {
		out = append(out, Diagnostic{Code: fallback, Message: "document validation failed without details"})
	}
	return out
}

func runtimeDiagnostics(valid bool, errs []*validatorerrors.ValidationError) []runtimevalidation.Diagnostic {
	if valid {
		return nil
	}
	out := make([]runtimevalidation.Diagnostic, 0, len(errs))
	for _, issue := range errs {
		if issue == nil {
			continue
		}
		code := issue.ValidationType
		if issue.ValidationSubType != "" {
			code += "." + issue.ValidationSubType
		}
		message := strings.TrimSpace(issue.Message)
		if issue.Reason != "" {
			message = strings.TrimSpace(message + ": " + issue.Reason)
		}
		out = append(out, runtimevalidation.Diagnostic{Code: code, Message: message, Path: issue.SpecPath, Line: issue.SpecLine, Column: issue.SpecCol})
	}
	return out
}
