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
	StatusResponseInvalid  Status = "response_invalid"
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

// Validator checks observed HTTP requests and responses against a contract.
type Validator interface {
	ValidateRequest(*http.Request) ValidationResult
	ValidateResponse(*http.Request, *http.Response) ValidationResult
}
