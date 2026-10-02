package engine

import (
	"encoding/json"
	"fmt"
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
			method, path = contract.NormalizeMethod(method), contract.NormalizePath(path)
			key := contract.OperationKey(method, path)
			op := graph.Operations[key]
			if op == nil {
				state := contract.OperationSourceOnly
				if fact.Status == string(contract.OperationUnresolved) {
					state = contract.OperationUnresolved
				}
				op = &contract.Operation{Key: key, Method: method, Path: path, State: state, Confidence: fact.Confidence, Parameters: map[string][]contract.Candidate[contract.Parameter]{}, Responses: map[string][]contract.Candidate[contract.Response]{}}
				graph.Operations[key] = op
			}
			if op.Confidence == 0 || fact.Confidence < op.Confidence {
				op.Confidence = fact.Confidence
			}
			op.OperationID, op.Tags, op.Gaps, op.Security = payload.OperationID, append([]string(nil), payload.Tags...), append([]string(nil), payload.Gaps...), append([]string(nil), payload.Security...)
			status := candidateStatus(fact)
			evidence := contractEvidence(fact.Evidence, fact.Source)
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
				value := contract.Response{Status: itoa(response.Status), Description: response.Description, ContentType: response.ContentType, Schema: response.SchemaType, Envelope: response.EnvelopeType, DataField: response.DataField, MapValueType: response.MapValueType, HasBody: response.HasBody || response.SchemaType != "" || response.ContentType != "", Raw: response.Raw, Failure: response.Failure, Sink: response.Sink, ErrSource: response.ErrSource}
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

func contractEvidence(items []facts.Evidence, factSource facts.FactSource) []contract.Evidence {
	out := make([]contract.Evidence, 0, len(items))
	for _, item := range items {
		source := contract.SourceCode
		if item.Source != "" {
			source = contract.EvidenceSource(item.Source)
		} else if factSource == facts.SourceLLM {
			source = contract.SourceLLM
		} else if factSource == facts.SourceRuntime {
			source = contract.SourceRuntime
		} else if factSource == facts.SourceHuman {
			source = contract.SourceHuman
		}
		out = append(out, contract.Evidence{Source: source, Location: contract.Location{File: item.File, StartLine: item.StartLine, EndLine: item.EndLine}, Summary: item.Quote, Strength: contract.StrengthDirect})
	}
	return out
}

// mergeContractGraphs combines an imported document graph with the source graph
// before compilation. Candidates remain separate so the compiler can preserve
// same-status response variants and graphStats can report conflicts.
func mergeContractGraphs(dst, src *contract.Graph) {
	if dst == nil || src == nil {
		return
	}
	if dst.Operations == nil {
		dst.Operations = map[string]*contract.Operation{}
	}
	if dst.Schemas == nil {
		dst.Schemas = map[string]*contract.Schema{}
	}
	for _, diagnostic := range src.Diagnostics {
		dst.Diagnostics = append(dst.Diagnostics, diagnostic)
	}
	for name, incoming := range src.Schemas {
		target := name
		if current, ok := dst.Schemas[name]; ok && !sameJSON(current, incoming) {
			target = "source." + strings.ReplaceAll(name, "/", "_")
			dst.Diagnostics = append(dst.Diagnostics, contract.Diagnostic{Code: "schema_name_collision", Message: fmt.Sprintf("schema %q differs between sources; namespaced as %q", name, target)})
		}
		if _, ok := dst.Schemas[target]; !ok {
			dst.Schemas[target] = incoming
		}
		if target != name {
			for _, operation := range src.Operations {
				if operation == nil {
					continue
				}
				for status, candidates := range operation.Responses {
					for i := range candidates {
						if candidates[i].Value.Schema == name {
							candidates[i].Value.Schema = target
						}
					}
					operation.Responses[status] = candidates
				}
				for i := range operation.RequestBody {
					if operation.RequestBody[i].Value.Schema == name {
						operation.RequestBody[i].Value.Schema = target
					}
				}
			}
		}
	}
	for key, incoming := range src.Operations {
		if incoming == nil {
			continue
		}
		current, ok := dst.Operations[key]
		if !ok {
			dst.Operations[key] = incoming
			continue
		}
		current.State = contract.OperationMatched
		if current.Confidence == 0 || incoming.Confidence < current.Confidence {
			current.Confidence = incoming.Confidence
		}
		current.Evidence = append(current.Evidence, incoming.Evidence...)
		if current.OperationID == "" {
			current.OperationID = incoming.OperationID
		}
		if current.Summary == "" {
			current.Summary = incoming.Summary
		}
		if current.Description == "" {
			current.Description = incoming.Description
		}
		current.Tags = append(current.Tags, incoming.Tags...)
		for name, candidates := range incoming.Parameters {
			current.Parameters[name] = append(current.Parameters[name], candidates...)
		}
		current.RequestBody = append(current.RequestBody, incoming.RequestBody...)
		if current.Responses == nil {
			current.Responses = map[string][]contract.Candidate[contract.Response]{}
		}
		for status, candidates := range incoming.Responses {
			current.Responses[status] = append(current.Responses[status], candidates...)
			for _, candidate := range candidates {
				current.Evidence = append(current.Evidence, candidate.Evidence...)
			}
		}
		if operationHasConflict(current) {
			current.State = contract.OperationConflict
		}
	}
}

func operationHasConflict(operation *contract.Operation) bool {
	if operation == nil {
		return false
	}
	for _, candidates := range operation.Parameters {
		if hasConflict(candidates) {
			return true
		}
	}
	if hasConflict(operation.RequestBody) {
		return true
	}
	for _, candidates := range operation.Responses {
		if hasConflict(candidates) {
			return true
		}
	}
	return false
}

func markGraphStates(graph *contract.Graph) {
	if graph == nil {
		return
	}
	for _, operation := range graph.Operations {
		if operation == nil {
			continue
		}
		if operationHasConflict(operation) {
			operation.State = contract.OperationConflict
		}
	}
}

func sameJSON(left, right any) bool {
	lb, lerr := json.Marshal(left)
	rb, rerr := json.Marshal(right)
	return lerr == nil && rerr == nil && string(lb) == string(rb)
}

func contractSchema(input *schema.Schema) *contract.Schema {
	if input == nil {
		return &contract.Schema{Unknown: true, UnknownReason: "nil source schema"}
	}
	result := &contract.Schema{Types: nil, Ref: contract.NormalizeSchemaRef(input.Ref), Format: input.Format, Nullable: input.Nullable, ContentEncoding: input.ContentEncoding, ContentMediaType: input.ContentMediaType, Description: input.Description, Required: append([]string(nil), input.Required...), ReadOnly: input.ReadOnly, WriteOnly: input.WriteOnly, Unknown: input.Unknown, UnknownReason: input.UnknownWhy}
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
	for _, diagnostic := range graph.Diagnostics {
		if strings.Contains(diagnostic.Code, "conflict") {
			conflicts++
		}
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

func graphHasGaps(graph *contract.Graph) bool {
	if graph == nil {
		return true
	}
	for _, operation := range graph.Operations {
		if operation != nil && (len(operation.Gaps) > 0 || operation.State == contract.OperationUnresolved || operation.State == contract.OperationConflict) {
			return true
		}
	}
	return false
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
