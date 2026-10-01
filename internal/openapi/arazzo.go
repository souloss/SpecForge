package openapi

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pb33f/libopenapi"
	arazzo "github.com/pb33f/libopenapi/arazzo"
	"gopkg.in/yaml.v3"
)

type Workflow struct {
	ID      string         `yaml:"workflowId" json:"workflow_id"`
	Summary string         `yaml:"summary,omitempty" json:"summary,omitempty"`
	Inputs  map[string]any `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Outputs map[string]any `yaml:"outputs,omitempty" json:"outputs,omitempty"`
	Steps   []Step         `yaml:"steps" json:"steps"`
}

type Step struct {
	ID              string          `yaml:"stepId" json:"step_id"`
	OperationRef    string          `yaml:"operationId,omitempty" json:"operation_id,omitempty"`
	OperationPath   string          `yaml:"operationPath,omitempty" json:"operation_path,omitempty"`
	Parameters      []StepParameter `yaml:"parameters,omitempty" json:"parameters,omitempty"`
	RequestBody     any             `yaml:"requestBody,omitempty" json:"request_body,omitempty"`
	SuccessCriteria []any           `yaml:"successCriteria,omitempty" json:"success_criteria,omitempty"`
	Outputs         map[string]any  `yaml:"outputs,omitempty" json:"outputs,omitempty"`
	FailureActions  []any           `yaml:"onFailure,omitempty" json:"failure_actions,omitempty"`
	Success         bool            `yaml:"-" json:"success"`
	Executed        bool            `yaml:"-" json:"executed"`
}

type StepParameter struct {
	Name  string `yaml:"name" json:"name"`
	In    string `yaml:"in,omitempty" json:"in,omitempty"`
	Value any    `yaml:"value,omitempty" json:"value,omitempty"`
}

type WorkflowGraph struct {
	Workflows []Workflow `yaml:"workflows" json:"workflows"`
}

type ArazzoResult struct {
	Graph       WorkflowGraph `json:"graph"`
	Diagnostics []Diagnostic  `json:"diagnostics,omitempty"`
}

// LoadArazzo parses and validates an Arazzo document while keeping its model
// separate from the Contract Graph and OpenAPI document model.
func LoadArazzo(path string) (*ArazzoResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	parsed, err := libopenapi.NewArazzoDocumentWithConfiguration(data, &libopenapi.ArazzoDocumentConfiguration{RetrievalURI: filepath.ToSlash(path)})
	if err != nil {
		return nil, fmt.Errorf("parse Arazzo: %w", err)
	}
	result := &ArazzoResult{}
	if validation := arazzo.Validate(parsed); validation != nil {
		for _, issue := range validation.Errors {
			if issue != nil {
				message := "Arazzo validation failed"
				if issue.Cause != nil {
					message = issue.Cause.Error()
				}
				result.Diagnostics = append(result.Diagnostics, Diagnostic{Code: "arazzo", Message: message, Path: issue.Path, Line: issue.Line, Column: issue.Column})
			}
		}
	}
	if err := yaml.Unmarshal(data, &result.Graph); err != nil {
		return nil, fmt.Errorf("decode Arazzo workflow graph: %w", err)
	}
	return result, nil
}
