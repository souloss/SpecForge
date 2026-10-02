package compiler

import (
	"fmt"
	"sort"

	"github.com/specforge/specforge/internal/contract"
	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/schema"
)

func factsFromGraph(graph *contract.Graph) ([]*facts.Fact, map[string]*schema.Schema) {
	if graph == nil {
		return nil, nil
	}
	var result []*facts.Fact
	keys := make([]string, 0, len(graph.Operations))
	for key := range graph.Operations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		operation := graph.Operations[key]
		if operation == nil {
			continue
		}
		payload := facts.ContractPayload{OperationID: operation.OperationID, Tags: append([]string(nil), operation.Tags...), Security: append([]string(nil), operation.Security...), Gaps: append([]string(nil), operation.Gaps...)}
		parameterNames := make([]string, 0, len(operation.Parameters))
		for name := range operation.Parameters {
			parameterNames = append(parameterNames, name)
		}
		sort.Strings(parameterNames)
		for _, name := range parameterNames {
			candidates := operation.Parameters[name]
			resolved, retained := contract.Resolve(candidates)
			if resolved.Status == contract.StatusConflict {
				payload.Gaps = append(payload.Gaps, "conflicting parameter candidates: "+name)
				continue
			}
			parameter := resolved.Value
			payload.Params = append(payload.Params, facts.ParamFact{Name: parameter.Name, In: parameter.In, Required: parameter.Required, Type: parameter.Type, Format: parameter.Format, Items: parameter.Items, Enum: append([]string(nil), parameter.Enum...), Description: parameter.Description, Origin: parameter.Origin})
			_ = retained
		}
		if resolved, _ := contract.Resolve(operation.RequestBody); resolved.Status != contract.StatusUnresolved && resolved.Status != contract.StatusConflict {
			body := resolved.Value
			payload.RequestBody = &facts.BodyFact{ContentType: body.ContentType, SchemaType: body.Schema, EnvelopeType: body.Envelope, DataField: body.DataField}
		} else if resolved.Status == contract.StatusConflict {
			payload.Gaps = append(payload.Gaps, "conflicting request body candidates")
		}
		statuses := make([]string, 0, len(operation.Responses))
		for status := range operation.Responses {
			statuses = append(statuses, status)
		}
		sort.Strings(statuses)
		for _, status := range statuses {
			candidates := operation.Responses[status]
			for _, candidate := range candidates {
				response := candidate.Value
				row := facts.ResponseFact{Status: atoi(status), Description: response.Description, ContentType: response.ContentType, SchemaType: response.Schema, EnvelopeType: response.Envelope, DataField: response.DataField, MapValueType: response.MapValueType, HasBody: response.HasBody, Raw: response.Raw, Failure: response.Failure, Sink: response.Sink, ErrSource: response.ErrSource}
				if response.CodeKnown {
					row.Envelope = &facts.Envelope{Code: response.Code, Msg: response.Description}
				}
				payload.Responses = append(payload.Responses, row)
			}
		}
		result = append(result, &facts.Fact{ID: graphFactID(operation), Kind: facts.KindContract, Value: payload, Source: facts.SourceStatic, Confidence: operationConfidence(operation), Evidence: graphOperationEvidence(operation), Status: graphStatus(operation.State)})
		if operation.Summary != "" || operation.Description != "" {
			result = append(result, &facts.Fact{ID: graphFactID(operation) + ":enrich", Kind: facts.KindEnrichment, Value: facts.EnrichmentPayload{Summary: operation.Summary, Description: operation.Description, Tags: append([]string(nil), operation.Tags...)}, Source: facts.SourceStatic, Confidence: 1, Status: "verified"})
		}
	}
	schemas := make(map[string]*schema.Schema, len(graph.Schemas))
	schemaNames := make([]string, 0, len(graph.Schemas))
	for name := range graph.Schemas {
		schemaNames = append(schemaNames, name)
	}
	sort.Strings(schemaNames)
	for _, name := range schemaNames {
		item := graph.Schemas[name]
		schemas[name] = schemaFromGraph(item)
		result = append(result, &facts.Fact{ID: "schema:" + name, Kind: facts.KindSchema, Value: facts.SchemaPayload{Schema: schemas[name]}, Source: facts.SourceStatic, Confidence: 1, Status: "verified"})
	}
	return result, schemas
}

func graphFactID(operation *contract.Operation) string {
	return "contract:op:" + operation.Method + ":" + operation.Path
}

func operationConfidence(operation *contract.Operation) float64 {
	confidence := operation.Confidence
	if confidence == 0 {
		confidence = 1
	}
	for _, candidates := range operation.Parameters {
		for _, candidate := range candidates {
			if candidate.Confidence < confidence {
				confidence = candidate.Confidence
			}
		}
	}
	return confidence
}

func graphEvidence(items []contract.Evidence) []facts.Evidence {
	out := make([]facts.Evidence, 0, len(items))
	for _, item := range items {
		out = append(out, facts.Evidence{File: item.Location.File, StartLine: item.Location.StartLine, EndLine: item.Location.EndLine, Quote: item.Summary, Source: string(item.Source)})
	}
	return out
}

func graphOperationEvidence(operation *contract.Operation) []facts.Evidence {
	if operation == nil {
		return nil
	}
	items := append([]contract.Evidence(nil), operation.Evidence...)
	for _, candidates := range operation.Parameters {
		for _, candidate := range candidates {
			items = append(items, candidate.Evidence...)
		}
	}
	for _, candidate := range operation.RequestBody {
		items = append(items, candidate.Evidence...)
	}
	for _, candidates := range operation.Responses {
		for _, candidate := range candidates {
			items = append(items, candidate.Evidence...)
		}
	}
	return graphEvidence(items)
}

func graphStatus(state contract.OperationState) string {
	if state == "" {
		return "verified"
	}
	return string(state)
}

func schemaFromGraph(input *contract.Schema) *schema.Schema {
	if input == nil {
		return nil
	}
	result := &schema.Schema{Type: first(input.Types), Format: input.Format, Ref: contract.NormalizeSchemaRef(input.Ref), Nullable: input.Nullable, ContentEncoding: input.ContentEncoding, ContentMediaType: input.ContentMediaType, Description: input.Description, Required: append([]string(nil), input.Required...), ReadOnly: input.ReadOnly, WriteOnly: input.WriteOnly, Unknown: input.Unknown, UnknownWhy: input.UnknownReason, Discriminator: input.Discriminator, Default: input.Default, Examples: append([]any(nil), input.Examples...)}
	for _, value := range input.Enum {
		result.Enum = append(result.Enum, fmt.Sprint(value))
	}
	propertyNames := make([]string, 0, len(input.Properties))
	for name := range input.Properties {
		propertyNames = append(propertyNames, name)
	}
	sort.Strings(propertyNames)
	for _, name := range propertyNames {
		property := input.Properties[name]
		result.Props = append(result.Props, schema.Prop{Name: name, Schema: schemaFromGraph(property)})
	}
	for _, name := range result.Required {
		for index := range result.Props {
			if result.Props[index].Name == name {
				result.Props[index].Required = true
			}
		}
	}
	if input.Items != nil {
		result.Items = schemaFromGraph(input.Items)
	}
	for _, branch := range input.AllOf {
		result.AllOf = append(result.AllOf, schemaFromGraph(branch))
	}
	for _, branch := range input.OneOf {
		result.OneOf = append(result.OneOf, schemaFromGraph(branch))
	}
	for _, branch := range input.AnyOf {
		result.AnyOf = append(result.AnyOf, schemaFromGraph(branch))
	}
	if input.Not != nil {
		result.Not = schemaFromGraph(input.Not)
	}
	if input.AdditionalProperties != nil {
		result.Additional = *input.AdditionalProperties
	}
	return result
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func atoi(value string) int {
	var result int
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0
		}
		result = result*10 + int(r-'0')
	}
	return result
}
