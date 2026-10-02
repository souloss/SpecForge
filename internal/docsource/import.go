// Package docsource imports explicit structured human API facts.
package docsource

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/specforge/specforge/internal/contract"
	"gopkg.in/yaml.v3"
)

type File struct {
	Operations []Operation `yaml:"operations" json:"operations"`
}
type Operation struct {
	Method      string              `yaml:"method" json:"method"`
	Path        string              `yaml:"path" json:"path"`
	Summary     string              `yaml:"summary,omitempty" json:"summary,omitempty"`
	Description string              `yaml:"description,omitempty" json:"description,omitempty"`
	Tags        []string            `yaml:"tags,omitempty" json:"tags,omitempty"`
	Parameters  []Parameter         `yaml:"parameters,omitempty" json:"parameters,omitempty"`
	Responses   map[string]Response `yaml:"responses" json:"responses"`
}
type Parameter struct {
	Name     string `yaml:"name" json:"name"`
	In       string `yaml:"in" json:"in"`
	Required bool   `yaml:"required,omitempty" json:"required,omitempty"`
	Type     string `yaml:"type,omitempty" json:"type,omitempty"`
	Format   string `yaml:"format,omitempty" json:"format,omitempty"`
}
type Response struct {
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	ContentType string `yaml:"content_type,omitempty" json:"content_type,omitempty"`
	Schema      string `yaml:"schema,omitempty" json:"schema,omitempty"`
}

// Import reads structured documentation into declared Contract Graph candidates.
func Import(path string) (*contract.Graph, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var input File
	if err := yaml.Unmarshal(data, &input); err != nil {
		return nil, fmt.Errorf("decode documentation %q: %w", path, err)
	}
	graph := contract.New()
	for index, item := range input.Operations {
		method := contract.NormalizeMethod(item.Method)
		item.Path = contract.NormalizePath(item.Path)
		if method == "" || !strings.HasPrefix(item.Path, "/") {
			return nil, fmt.Errorf("%s: operations[%d] requires method and absolute path", path, index)
		}
		if len(item.Responses) == 0 {
			return nil, fmt.Errorf("%s: operations[%d] requires responses", path, index)
		}
		key := contract.OperationKey(method, item.Path)
		if _, exists := graph.Operations[key]; exists {
			return nil, fmt.Errorf("%s: duplicate operation %s", path, key)
		}
		op := &contract.Operation{Key: key, Method: method, Path: item.Path, OperationID: method + item.Path, Summary: item.Summary, Description: item.Description, Tags: append([]string(nil), item.Tags...), State: contract.OperationDocumentOnly, Confidence: .7, Parameters: map[string][]contract.Candidate[contract.Parameter]{}, Responses: map[string][]contract.Candidate[contract.Response]{}}
		op.Evidence = []contract.Evidence{{Source: contract.SourceDocumentation, Location: contract.Location{File: path, Document: path, JSONPath: fmt.Sprintf("$.operations[%d]", index)}, Strength: contract.StrengthDirect}}
		for parameterIndex, parameter := range item.Parameters {
			value := contract.Parameter{Name: parameter.Name, In: parameter.In, Required: parameter.Required, Type: parameter.Type, Format: parameter.Format}
			location := fmt.Sprintf("$.operations[%d].parameters[%d]", index, parameterIndex)
			op.Parameters[parameter.Name] = append(op.Parameters[parameter.Name], contract.Candidate[contract.Parameter]{Value: value, Confidence: .7, Status: contract.StatusDeclared, Evidence: []contract.Evidence{{Source: contract.SourceDocumentation, Location: contract.Location{File: path, Document: path, JSONPath: location}, Strength: contract.StrengthDirect}}})
		}
		statuses := make([]string, 0, len(item.Responses))
		for status := range item.Responses {
			statuses = append(statuses, status)
		}
		sort.Strings(statuses)
		for _, status := range statuses {
			code, parseErr := strconv.Atoi(status)
			if parseErr != nil || code < 100 || code > 599 {
				return nil, fmt.Errorf("%s: operation %s has invalid response status %q", path, key, status)
			}
			response := item.Responses[status]
			location := fmt.Sprintf("$.operations[%d].responses[%s]", index, status)
			op.Responses[status] = append(op.Responses[status], contract.Candidate[contract.Response]{Value: contract.Response{Status: status, Description: response.Description, ContentType: response.ContentType, Schema: response.Schema, HasBody: response.Schema != "" || response.ContentType != "", Raw: true}, Confidence: .7, Status: contract.StatusDeclared, Evidence: []contract.Evidence{{Source: contract.SourceDocumentation, Location: contract.Location{File: path, Document: path, JSONPath: location}, Strength: contract.StrengthDirect}}})
		}
		graph.Operations[key] = op
	}
	return graph, nil
}
