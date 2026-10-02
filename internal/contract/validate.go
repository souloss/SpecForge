package contract

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var pathParameterPattern = regexp.MustCompile(`\{([^}/]+)\}`)

// ValidateGraph checks source-independent invariants before OpenAPI compile.
// It reports all findings in stable order and never chooses between conflicting
// candidates; policy about whether a finding is fatal belongs to the engine/CLI.
func ValidateGraph(graph *Graph) []Diagnostic {
	if graph == nil {
		return []Diagnostic{{Code: "graph_missing", Message: "contract graph is nil"}}
	}
	diagnostics := append([]Diagnostic(nil), graph.Diagnostics...)
	keys := make([]string, 0, len(graph.Operations))
	for key := range graph.Operations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		op := graph.Operations[key]
		if op == nil {
			diagnostics = append(diagnostics, Diagnostic{Code: "operation_missing", Message: fmt.Sprintf("operation %q is nil", key)})
			continue
		}
		if op.Method == "" || op.Path == "" || OperationKey(op.Method, op.Path) != key {
			diagnostics = append(diagnostics, Diagnostic{Code: "operation_identity", Message: fmt.Sprintf("operation %q has inconsistent method/path identity", key)})
		}
		paramNames := map[string]bool{}
		for name, candidates := range op.Parameters {
			if len(candidates) == 0 {
				continue
			}
			param := candidates[0].Value
			paramNames[param.In+":"+param.Name] = true
			if !validParameterIn(param.In) {
				diagnostics = append(diagnostics, Diagnostic{Code: "parameter_location", Message: fmt.Sprintf("%s %s parameter %q has invalid location %q", op.Method, op.Path, name, param.In)})
			}
			if len(candidates) > 1 && hasCandidateConflict(candidates) {
				diagnostics = append(diagnostics, Diagnostic{Code: "parameter_conflict", Message: fmt.Sprintf("%s %s parameter %q has conflicting candidates", op.Method, op.Path, name)})
			}
			if ref := NormalizeSchemaRef(param.Schema); ref != "" && graph.Schemas[ref] == nil {
				diagnostics = append(diagnostics, Diagnostic{Code: "schema_reference_missing", Message: fmt.Sprintf("%s %s parameter %q references unknown schema %q", op.Method, op.Path, name, ref)})
			}
		}
		for _, match := range pathParameterPattern.FindAllStringSubmatch(op.Path, -1) {
			if len(match) == 2 && !paramNames["path:"+match[1]] {
				diagnostics = append(diagnostics, Diagnostic{Code: "path_parameter_missing", Message: fmt.Sprintf("%s %s path parameter %q has no path parameter candidate", op.Method, op.Path, match[1])})
			}
		}
		if len(op.RequestBody) > 1 && hasCandidateConflict(op.RequestBody) {
			diagnostics = append(diagnostics, Diagnostic{Code: "request_body_conflict", Message: fmt.Sprintf("%s %s has conflicting request body candidates", op.Method, op.Path)})
		}
		for _, candidate := range op.RequestBody {
			if ref := NormalizeSchemaRef(candidate.Value.Schema); ref != "" && graph.Schemas[ref] == nil {
				diagnostics = append(diagnostics, Diagnostic{Code: "schema_reference_missing", Message: fmt.Sprintf("%s %s request body references unknown schema %q", op.Method, op.Path, ref)})
			}
		}
		statuses := make([]string, 0, len(op.Responses))
		for status := range op.Responses {
			statuses = append(statuses, status)
		}
		sort.Strings(statuses)
		for _, status := range statuses {
			code, err := strconv.Atoi(status)
			if err != nil || code < 100 || code > 599 {
				diagnostics = append(diagnostics, Diagnostic{Code: "response_status", Message: fmt.Sprintf("%s %s has invalid response status %q", op.Method, op.Path, status)})
			}
			if len(op.Responses[status]) > 1 && hasCandidateConflict(op.Responses[status]) {
				diagnostics = append(diagnostics, Diagnostic{Code: "response_conflict", Message: fmt.Sprintf("%s %s response %s has conflicting candidates", op.Method, op.Path, status)})
			}
			for _, candidate := range op.Responses[status] {
				if ref := NormalizeSchemaRef(candidate.Value.Schema); ref != "" && graph.Schemas[ref] == nil {
					diagnostics = append(diagnostics, Diagnostic{Code: "schema_reference_missing", Message: fmt.Sprintf("%s %s response %s references unknown schema %q", op.Method, op.Path, status, ref)})
				}
			}
		}
		if op.State == OperationConflict {
			diagnostics = append(diagnostics, Diagnostic{Code: "operation_conflict", Message: fmt.Sprintf("%s %s is marked conflict", op.Method, op.Path)})
		}
		if op.State == OperationUnresolved || len(op.Gaps) > 0 {
			diagnostics = append(diagnostics, Diagnostic{Code: "operation_unresolved", Message: fmt.Sprintf("%s %s has unresolved gaps", op.Method, op.Path)})
		}
	}
	for name, schema := range graph.Schemas {
		if schema != nil && schema.Unknown {
			diagnostics = append(diagnostics, Diagnostic{Code: "schema_unknown", Message: fmt.Sprintf("schema %q is unknown: %s", name, schema.UnknownReason)})
		}
	}
	sort.SliceStable(diagnostics, func(i, j int) bool {
		if diagnostics[i].Code != diagnostics[j].Code {
			return diagnostics[i].Code < diagnostics[j].Code
		}
		return diagnostics[i].Message < diagnostics[j].Message
	})
	return diagnostics
}

func validParameterIn(value string) bool {
	switch strings.ToLower(value) {
	case "path", "query", "header", "cookie":
		return true
	default:
		return false
	}
}

func hasCandidateConflict[T any](candidates []Candidate[T]) bool {
	if len(candidates) < 2 {
		return false
	}
	_, retained := Resolve(candidates)
	for _, candidate := range retained {
		if candidate.Status == StatusConflict {
			return true
		}
	}
	return false
}
