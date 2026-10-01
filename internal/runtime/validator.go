// Package runtime defines the stable boundary for validating observed HTTP traffic.
package runtime

import (
	"net/http"
)

// Status is the outcome of validating one observed request or response.
type Status string

const (
	StatusValid            Status = "valid"
	StatusOperationMissing Status = "operation_not_found"
	StatusRequestInvalid   Status = "request_invalid"
	StatusHandlerError     Status = "handler_error"
	StatusResponseInvalid  Status = "response_invalid"
	StatusSchemaUnresolved Status = "schema_unresolved"
	StatusInconclusive     Status = "inconclusive"
)

// Diagnostic is an implementation-independent validation finding.
type Diagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
}

// ValidationResult describes one sample. It deliberately carries no payload data.
type ValidationResult struct {
	Status      Status       `json:"status"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// RedactedSample contains only structural information about an observed HTTP
// exchange. Bodies and credential-bearing headers are deliberately omitted.
type RedactedSample struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Status      int    `json:"status,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	BodyBytes   int64  `json:"body_bytes,omitempty"`
	TraceID     string `json:"trace_id,omitempty"`
	ObservedAt  string `json:"observed_at,omitempty"`
}

// RedactRequest extracts safe request metadata without retaining payloads.
func RedactRequest(request *http.Request) RedactedSample {
	if request == nil {
		return RedactedSample{}
	}
	return RedactedSample{Method: request.Method, Path: request.URL.Path, ContentType: request.Header.Get("Content-Type")}
}

// RedactResponse extracts safe response metadata without retaining payloads.
func RedactResponse(response *http.Response) RedactedSample {
	if response == nil {
		return RedactedSample{}
	}
	return RedactedSample{Status: response.StatusCode, ContentType: response.Header.Get("Content-Type")}
}

// Validator checks observed HTTP requests and responses against a contract.
type Validator interface {
	ValidateRequest(*http.Request) ValidationResult
	ValidateResponse(*http.Request, *http.Response) ValidationResult
}
