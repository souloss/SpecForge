package openapi

import (
	"sort"

	"github.com/specforge/specforge/internal/contract"
)

// EvaluationDocument is a small, adapter-owned projection used by eval. It
// contains no libopenapi types and is populated from the Contract Graph.
type EvaluationDocument struct {
	Paths      map[string]map[string]EvaluationOperation
	Components EvaluationComponents
}

type EvaluationComponents struct {
	Schemas map[string]EvaluationSchema
}

type EvaluationOperation struct {
	OperationID string
	Parameters  []EvaluationParameter
	RequestBody *EvaluationBody
	Responses   map[string]EvaluationResponse
}

type EvaluationParameter struct {
	Name     string
	In       string
	Required bool
	Schema   EvaluationSchema
}

type EvaluationBody struct {
	Content map[string]EvaluationMedia
}

type EvaluationMedia struct {
	Schema EvaluationSchema
}

type EvaluationResponse struct {
	Description string
	Content     map[string]EvaluationMedia
}

type EvaluationSchema struct {
	Ref                  string
	Type                 any
	Format               string
	Enum                 []any
	Props                map[string]EvaluationSchema
	Required             []string
	Items                *EvaluationSchema
	Nullable             bool
	AdditionalProperties any
	AllOf                []EvaluationSchema
}

// LoadEvaluation loads an OpenAPI 3 document through the shared adapter and
// returns the normalized view required by evaluation metrics.
func LoadEvaluation(path string) (*EvaluationDocument, error) {
	graph, err := Import(path)
	if err != nil {
		return nil, err
	}
	return evaluationDocument(graph), nil
}

func evaluationDocument(graph *contract.Graph) *EvaluationDocument {
	result := &EvaluationDocument{Paths: map[string]map[string]EvaluationOperation{}, Components: EvaluationComponents{Schemas: map[string]EvaluationSchema{}}}
	if graph == nil {
		return result
	}
	for name, schema := range graph.Schemas {
		result.Components.Schemas[name] = evaluationSchemaRef(schema, graph.Schemas)
	}
	keys := make([]string, 0, len(graph.Operations))
	for key := range graph.Operations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		op := graph.Operations[key]
		if op == nil {
			continue
		}
		method := lower(op.Method)
		if result.Paths[op.Path] == nil {
			result.Paths[op.Path] = map[string]EvaluationOperation{}
		}
		item := EvaluationOperation{OperationID: op.OperationID, Responses: map[string]EvaluationResponse{}}
		parameterNames := make([]string, 0, len(op.Parameters))
		for name := range op.Parameters {
			parameterNames = append(parameterNames, name)
		}
		sort.Strings(parameterNames)
		for _, name := range parameterNames {
			candidates := op.Parameters[name]
			if len(candidates) == 0 {
				continue
			}
			parameter := candidates[0].Value
			item.Parameters = append(item.Parameters, EvaluationParameter{Name: parameter.Name, In: parameter.In, Required: parameter.Required, Schema: evaluationSchemaName(parameter.Schema, graph.Schemas)})
		}
		if len(op.RequestBody) > 0 {
			body := op.RequestBody[0].Value
			item.RequestBody = &EvaluationBody{Content: map[string]EvaluationMedia{body.ContentType: {Schema: evaluationSchemaName(body.Schema, graph.Schemas)}}}
		}
		statuses := make([]string, 0, len(op.Responses))
		for status := range op.Responses {
			statuses = append(statuses, status)
		}
		sort.Strings(statuses)
		for _, status := range statuses {
			candidates := op.Responses[status]
			if len(candidates) == 0 {
				continue
			}
			response := candidates[0].Value
			item.Responses[status] = EvaluationResponse{Description: response.Description, Content: map[string]EvaluationMedia{response.ContentType: {Schema: evaluationSchemaName(response.Schema, graph.Schemas)}}}
		}
		result.Paths[op.Path][method] = item
	}
	return result
}

func evaluationSchemaName(name string, schemas map[string]*contract.Schema) EvaluationSchema {
	if name == "" {
		return EvaluationSchema{}
	}
	if schema, ok := schemas[name]; ok {
		return evaluationSchemaRef(schema, schemas)
	}
	return EvaluationSchema{Ref: "#/components/schemas/" + name}
}

func evaluationSchemaRef(input *contract.Schema, schemas map[string]*contract.Schema) EvaluationSchema {
	if input == nil {
		return EvaluationSchema{}
	}
	result := EvaluationSchema{Ref: input.Ref, Type: append([]string(nil), input.Types...), Format: input.Format, Required: append([]string(nil), input.Required...), Nullable: input.Nullable}
	if len(input.Types) == 1 {
		result.Type = input.Types[0]
	}
	for _, value := range input.Enum {
		result.Enum = append(result.Enum, value)
	}
	if len(input.Properties) > 0 {
		result.Props = map[string]EvaluationSchema{}
	}
	propertyNames := make([]string, 0, len(input.Properties))
	for name := range input.Properties {
		propertyNames = append(propertyNames, name)
	}
	sort.Strings(propertyNames)
	for _, name := range propertyNames {
		property := input.Properties[name]
		result.Props[name] = evaluationSchemaRef(property, schemas)
	}
	if input.Items != nil {
		item := evaluationSchemaRef(input.Items, schemas)
		result.Items = &item
	}
	if input.AdditionalProperties != nil {
		result.AdditionalProperties = *input.AdditionalProperties
	}
	for _, branch := range input.AllOf {
		result.AllOf = append(result.AllOf, evaluationSchemaRef(branch, schemas))
	}
	return result
}

func lower(value string) string {
	if value == "" {
		return ""
	}
	if value[0] >= 'A' && value[0] <= 'Z' {
		return string(value[0]+('a'-'A')) + value[1:]
	}
	return value
}
