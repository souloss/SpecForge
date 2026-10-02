package openapi

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pb33f/go-yaml"
	"github.com/pb33f/libopenapi/datamodel/high/base"
	"github.com/pb33f/libopenapi/datamodel/high/v3"
	"github.com/pb33f/libopenapi/orderedmap"

	"github.com/specforge/specforge/internal/contract"
)

// Import loads an OpenAPI document and converts it into the source-independent
// Contract Graph. libopenapi types remain private to this adapter.
func Import(path string) (*contract.Graph, error) {
	doc, err := Load(path)
	if err != nil {
		return nil, err
	}
	defer doc.Close()
	return importDocument(doc, path)
}

// ImportBytes converts an in-memory OpenAPI document into a Contract Graph.
func ImportBytes(data []byte, sourcePath string) (*contract.Graph, error) {
	doc, err := LoadBytes(data, sourcePath)
	if err != nil {
		return nil, err
	}
	defer doc.Close()
	return importDocument(doc, sourcePath)
}

func importDocument(doc *Document, sourcePath string) (*contract.Graph, error) {
	if diagnostics := doc.Validate(); len(diagnostics) > 0 {
		messages := make([]string, 0, len(diagnostics))
		for _, diagnostic := range diagnostics {
			location := diagnostic.Path
			if diagnostic.Line > 0 {
				location = fmt.Sprintf("%s:%d:%d", sourcePath, diagnostic.Line, diagnostic.Column)
			}
			message := diagnostic.Message
			if location != "" {
				message = location + ": " + message
			}
			messages = append(messages, message)
		}
		return nil, fmt.Errorf("OpenAPI document failed validation: %s", strings.Join(messages, "; "))
	}
	model, err := doc.document.BuildV3Model()
	if err != nil {
		return nil, fmt.Errorf("build OpenAPI model for import: %w", err)
	}
	if model == nil {
		return nil, fmt.Errorf("build OpenAPI model for import: empty model")
	}
	graph := contract.New()
	base := filepath.ToSlash(sourcePath)
	if model.Model.Components != nil && model.Model.Components.Schemas != nil {
		for name, proxy := range model.Model.Components.Schemas.FromOldest() {
			graph.Schemas[name] = importSchema(proxy, map[string]bool{})
		}
	}
	if model.Model.Paths == nil || model.Model.Paths.PathItems == nil {
		return graph, nil
	}
	for path, item := range model.Model.Paths.PathItems.FromOldest() {
		for method, operation := range item.GetOperations().FromOldest() {
			if operation == nil {
				continue
			}
			method = contract.NormalizeMethod(method)
			path = contract.NormalizePath(path)
			key := contract.OperationKey(method, path)
			op := &contract.Operation{
				Key: key, Path: path, Method: method,
				OperationID: operation.OperationId, Summary: operation.Summary,
				Description: operation.Description, Tags: append([]string(nil), operation.Tags...),
				State: contract.OperationDocumentOnly, Confidence: 0.95,
				Parameters: map[string][]contract.Candidate[contract.Parameter]{},
				Responses:  map[string][]contract.Candidate[contract.Response]{},
			}
			op.Evidence = []contract.Evidence{openAPIEvidence(base, "$.paths["+quotePath(path)+"]."+strings.ToLower(method))}
			for index, parameter := range operation.Parameters {
				if parameter == nil {
					continue
				}
				param := contract.Parameter{Name: parameter.Name, In: parameter.In}
				if parameter.Required != nil {
					param.Required = *parameter.Required
				}
				param.Schema = registerSchema(graph, key+":param:"+parameter.In+":"+parameter.Name, parameter.Schema)
				jsonPath := fmt.Sprintf("$.paths[%s].%s.parameters[%d]", quotePath(path), strings.ToLower(method), index)
				op.Parameters[parameter.Name] = append(op.Parameters[parameter.Name], contract.Candidate[contract.Parameter]{
					Value: param, Confidence: 0.95, Status: contract.StatusDeclared,
					Evidence: []contract.Evidence{openAPIEvidence(base, jsonPath)},
				})
			}
			if body := operation.RequestBody; body != nil {
				contentType, media := firstMedia(body.Content)
				request := contract.RequestBody{ContentType: contentType, Required: body.Required != nil && *body.Required}
				if media != nil {
					request.Schema = registerSchema(graph, key+":request", media.Schema)
				}
				op.RequestBody = append(op.RequestBody, contract.Candidate[contract.RequestBody]{
					Value: request, Confidence: 0.95, Status: contract.StatusDeclared,
					Evidence: []contract.Evidence{openAPIEvidence(base, "$.paths["+quotePath(path)+"]."+strings.ToLower(method)+".requestBody")},
				})
			}
			if operation.Responses != nil {
				for status, response := range operation.Responses.Codes.FromOldest() {
					if response == nil {
						continue
					}
					item := contract.Response{Status: status, Description: response.Description, Raw: true}
					contentType, media := firstMedia(response.Content)
					item.ContentType = contentType
					if media != nil {
						item.HasBody = true
						item.Schema = registerSchema(graph, key+":response:"+status, media.Schema)
					}
					jsonPath := "$.paths[" + quotePath(path) + "]." + strings.ToLower(method) + ".responses[" + quotePath(status) + "]"
					op.Responses[status] = append(op.Responses[status], contract.Candidate[contract.Response]{
						Value: item, Confidence: 0.95, Status: contract.StatusDeclared,
						Evidence: []contract.Evidence{openAPIEvidence(base, jsonPath)},
					})
				}
			}
			if err := graph.AddOperation(op); err != nil {
				return nil, err
			}
		}
	}
	return graph, nil
}

func openAPIEvidence(document, path string) contract.Evidence {
	return contract.Evidence{Source: contract.SourceOpenAPI, Location: contract.Location{File: document, Document: document, JSONPath: path}, Strength: contract.StrengthDirect}
}

func quotePath(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func firstMedia(content *orderedmap.Map[string, *v3.MediaType]) (string, *v3.MediaType) {
	if content == nil {
		return "", nil
	}
	for name, media := range content.FromOldest() {
		return name, media
	}
	return "", nil
}

func registerSchema(graph *contract.Graph, key string, proxy *base.SchemaProxy) string {
	if proxy == nil {
		return ""
	}
	if proxy.IsReference() {
		return strings.TrimPrefix(proxy.GetReference(), "#/components/schemas/")
	}
	name := "inline_" + stableName(key)
	if _, exists := graph.Schemas[name]; !exists {
		graph.Schemas[name] = importSchema(proxy, map[string]bool{})
	}
	return name
}

func stableName(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func importSchema(proxy *base.SchemaProxy, stack map[string]bool) *contract.Schema {
	if proxy == nil {
		return &contract.Schema{Unknown: true, UnknownReason: "missing schema"}
	}
	result := &contract.Schema{}
	if proxy.IsReference() {
		result.Ref = proxy.GetReference()
		if stack[result.Ref] {
			return result
		}
		stack[result.Ref] = true
		defer delete(stack, result.Ref)
	}
	schema := proxy.Schema()
	if schema == nil {
		result.Unknown = true
		result.UnknownReason = "schema could not be resolved"
		return result
	}
	result.Types = append(result.Types, schema.Type...)
	result.Format = schema.Format
	result.ContentEncoding = schema.ContentEncoding
	result.ContentMediaType = schema.ContentMediaType
	result.Description = schema.Description
	result.Required = append(result.Required, schema.Required...)
	result.ReadOnly = schema.ReadOnly != nil && *schema.ReadOnly
	result.WriteOnly = schema.WriteOnly != nil && *schema.WriteOnly
	result.Nullable = schema.Nullable != nil && *schema.Nullable
	if schema.Discriminator != nil {
		result.Discriminator = schema.Discriminator.PropertyName
	}
	for _, value := range schema.Enum {
		result.Enum = append(result.Enum, yamlValue(value))
	}
	if schema.Default != nil {
		result.Default = yamlValue(schema.Default)
	}
	for _, value := range schema.Examples {
		result.Examples = append(result.Examples, yamlValue(value))
	}
	if schema.Properties != nil {
		result.Properties = map[string]*contract.Schema{}
		names := make([]string, 0, schema.Properties.Len())
		for name := range schema.Properties.KeysFromOldest() {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			property := schema.Properties.GetOrZero(name)
			result.Properties[name] = importSchema(property, stack)
		}
	}
	if schema.Items != nil && schema.Items.IsA() {
		result.Items = importSchema(schema.Items.A, stack)
	}
	for _, branch := range schema.AllOf {
		result.AllOf = append(result.AllOf, importSchema(branch, stack))
	}
	for _, branch := range schema.OneOf {
		result.OneOf = append(result.OneOf, importSchema(branch, stack))
	}
	for _, branch := range schema.AnyOf {
		result.AnyOf = append(result.AnyOf, importSchema(branch, stack))
	}
	if schema.Not != nil {
		result.Not = importSchema(schema.Not, stack)
	}
	if schema.AdditionalProperties != nil {
		if schema.AdditionalProperties.IsB() {
			value := schema.AdditionalProperties.B
			result.AdditionalProperties = &value
		} else if schema.AdditionalProperties.IsA() {
			result.Unknown = true
			result.UnknownReason = "schema-valued additionalProperties requires an explicit projection"
		}
	}
	return result
}

func yamlValue(node *yaml.Node) any {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.ScalarNode:
		return node.Value
	case yaml.SequenceNode:
		values := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			values = append(values, yamlValue(child))
		}
		return values
	case yaml.MappingNode:
		values := map[string]any{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			values[node.Content[i].Value] = yamlValue(node.Content[i+1])
		}
		return values
	default:
		return node.Value
	}
}
