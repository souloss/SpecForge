package engine

import (
	"strconv"
	"strings"

	"github.com/specforge/specforge/internal/contract"
	"github.com/specforge/specforge/internal/facts"
	"github.com/specforge/specforge/internal/schema"
)

func graphFromFacts(items []*facts.Fact) *contract.Graph {
	graph := contract.New()
	for _, fact := range items {
		if fact == nil {
			continue
		}
		switch fact.Kind {
		case facts.KindSchema:
			if value, ok := fact.Schema(); ok {
				graph.Schemas[strings.TrimPrefix(fact.ID, "schema:")] = contractSchema(value)
			}
		case facts.KindContract:
			payload, ok := fact.Contract()
			if !ok {
				continue
			}
			method, path := opMethodPath(fact.ID)
			key := contract.OperationKey(method, path)
			op := graph.Operations[key]
			if op == nil {
				op = &contract.Operation{Key: key, Method: method, Path: path, State: contract.OperationSourceOnly, Confidence: fact.Confidence, Parameters: map[string][]contract.Candidate[contract.Parameter]{}, Responses: map[string][]contract.Candidate[contract.Response]{}}
				graph.Operations[key] = op
			}
			if op.Confidence == 0 || fact.Confidence < op.Confidence {
				op.Confidence = fact.Confidence
			}
			op.OperationID, op.Tags, op.Gaps, op.Security = payload.OperationID, append([]string(nil), payload.Tags...), append([]string(nil), payload.Gaps...), append([]string(nil), payload.Security...)
			status := candidateStatus(fact)
			evidence := contractEvidence(fact.Evidence)
			for _, parameter := range payload.Params {
				value := contract.Parameter{Name: parameter.Name, In: parameter.In, Required: parameter.Required, Type: parameter.Type, Format: parameter.Format, Items: parameter.Items, Enum: append([]string(nil), parameter.Enum...), Description: parameter.Description, Origin: parameter.Origin}
				op.Parameters[parameter.Name] = append(op.Parameters[parameter.Name], contract.Candidate[contract.Parameter]{Value: value, Evidence: evidence, Confidence: fact.Confidence, Status: status})
			}
			if payload.RequestBody != nil {
				body := payload.RequestBody
				value := contract.RequestBody{ContentType: body.ContentType, Schema: body.SchemaType, Envelope: body.EnvelopeType, DataField: body.DataField, Required: true}
				op.RequestBody = append(op.RequestBody, contract.Candidate[contract.RequestBody]{Value: value, Evidence: evidence, Confidence: fact.Confidence, Status: status})
			}
			for _, response := range payload.Responses {
				value := contract.Response{Status: itoa(response.Status), Schema: response.SchemaType, Envelope: response.EnvelopeType, DataField: response.DataField, MapValueType: response.MapValueType, HasBody: response.HasBody, Raw: response.Raw, Failure: response.Failure, Sink: response.Sink, ErrSource: response.ErrSource}
				if response.Envelope != nil {
					value.Code = response.Envelope.Code
					value.CodeKnown = true
					value.Description = response.Envelope.Msg
				}
				op.Responses[value.Status] = append(op.Responses[value.Status], contract.Candidate[contract.Response]{Value: value, Evidence: evidence, Confidence: fact.Confidence, Status: status})
			}
		case facts.KindEnrichment:
			payload, ok := fact.Enrichment()
			if !ok {
				continue
			}
			method, path := opMethodPath(strings.TrimSuffix(fact.ID, ":enrich"))
			key := contract.OperationKey(method, path)
			if operation := graph.Operations[key]; operation != nil {
				operation.Summary, operation.Description = payload.Summary, payload.Description
			}
		}
	}
	return graph
}

func candidateStatus(fact *facts.Fact) contract.CandidateStatus {
	if fact.Source == facts.SourceLLM {
		return contract.StatusInferred
	}
	if fact.Source == facts.SourceRuntime {
		return contract.StatusObserved
	}
	if fact.Status == "" || fact.Status == "verified" {
		return contract.StatusVerified
	}
	return contract.CandidateStatus(fact.Status)
}

func contractEvidence(items []facts.Evidence) []contract.Evidence {
	out := make([]contract.Evidence, 0, len(items))
	for _, item := range items {
		out = append(out, contract.Evidence{Source: contract.SourceCode, Location: contract.Location{File: item.File, StartLine: item.StartLine, EndLine: item.EndLine}, Summary: item.Quote, Strength: contract.StrengthDirect})
	}
	return out
}

func contractSchema(input *schema.Schema) *contract.Schema {
	if input == nil {
		return &contract.Schema{Unknown: true, UnknownReason: "nil source schema"}
	}
	result := &contract.Schema{Types: nil, Ref: input.Ref, Format: input.Format, Nullable: input.Nullable, Description: input.Description, Required: append([]string(nil), input.Required...), ReadOnly: input.ReadOnly, WriteOnly: input.WriteOnly, Unknown: input.Unknown, UnknownReason: input.UnknownWhy}
	if input.Type != "" {
		result.Types = []string{input.Type}
	}
	result.Enum = make([]any, 0, len(input.Enum))
	for _, value := range input.Enum {
		result.Enum = append(result.Enum, value)
	}
	for _, property := range input.Props {
		if result.Properties == nil {
			result.Properties = map[string]*contract.Schema{}
		}
		result.Properties[property.Name] = contractSchema(property.Schema)
	}
	if input.Items != nil {
		result.Items = contractSchema(input.Items)
	}
	for _, branch := range input.AllOf {
		result.AllOf = append(result.AllOf, contractSchema(branch))
	}
	for _, branch := range input.OneOf {
		result.OneOf = append(result.OneOf, contractSchema(branch))
	}
	for _, branch := range input.AnyOf {
		result.AnyOf = append(result.AnyOf, contractSchema(branch))
	}
	if input.Not != nil {
		result.Not = contractSchema(input.Not)
	}
	result.AdditionalProperties = boolPtr(input.Additional)
	return result
}

func itoa(value int) string {
	return strconv.Itoa(value)
}

func boolPtr(value bool) *bool { return &value }

func graphStats(graph *contract.Graph) (conflicts, unknownSchemas int) {
	if graph == nil {
		return 0, 0
	}
	for _, operation := range graph.Operations {
		for _, candidates := range operation.Parameters {
			if _, retained := contract.Resolve(candidates); len(retained) > 1 && hasConflict(candidates) {
				conflicts++
			}
		}
		if _, retained := contract.Resolve(operation.RequestBody); len(retained) > 1 && hasConflict(retained) {
			conflicts++
		}
		for _, candidates := range operation.Responses {
			if len(candidates) > 1 && hasConflict(candidates) {
				conflicts++
			}
		}
	}
	for _, schema := range graph.Schemas {
		if schema != nil && schema.Unknown {
			unknownSchemas++
		}
	}
	return conflicts, unknownSchemas
}

func hasConflict[T any](candidates []contract.Candidate[T]) bool {
	if len(candidates) < 2 {
		return false
	}
	_, retained := contract.Resolve(candidates)
	for _, candidate := range retained {
		if candidate.Status == contract.StatusConflict {
			return true
		}
	}
	return false
}
