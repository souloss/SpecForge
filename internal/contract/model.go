// Package contract defines SpecForge's source independent contract graph.
// OpenAPI library types must be converted at the openapi package boundary.
package contract

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type EvidenceSource string

const (
	SourceCode          EvidenceSource = "source"
	SourceOpenAPI       EvidenceSource = "openapi"
	SourceRuntime       EvidenceSource = "runtime"
	SourceDocumentation EvidenceSource = "documentation"
	SourceHuman         EvidenceSource = "human"
	SourceLLM           EvidenceSource = "llm"
)

type EvidenceStrength string

const (
	StrengthDirect  EvidenceStrength = "direct"
	StrengthDerived EvidenceStrength = "derived"
	StrengthWeak    EvidenceStrength = "weak"
)

type Evidence struct {
	Source   EvidenceSource   `json:"source"`
	Location Location         `json:"location"`
	Summary  string           `json:"summary,omitempty"`
	Strength EvidenceStrength `json:"strength"`
}

type Location struct {
	File       string `json:"file,omitempty"`
	StartLine  int    `json:"start_line,omitempty"`
	StartCol   int    `json:"start_col,omitempty"`
	EndLine    int    `json:"end_line,omitempty"`
	EndCol     int    `json:"end_col,omitempty"`
	Package    string `json:"package,omitempty"`
	Symbol     string `json:"symbol,omitempty"`
	Document   string `json:"document,omitempty"`
	JSONPath   string `json:"json_path,omitempty"`
	TraceID    string `json:"trace_id,omitempty"`
	ObservedAt string `json:"observed_at,omitempty"`
}

type CandidateStatus string

const (
	StatusVerified     CandidateStatus = "verified"
	StatusDeclared     CandidateStatus = "declared"
	StatusObserved     CandidateStatus = "observed"
	StatusInferred     CandidateStatus = "inferred"
	StatusConflict     CandidateStatus = "conflict"
	StatusUnresolved   CandidateStatus = "unresolved"
	StatusInconclusive CandidateStatus = "inconclusive"
)

type Candidate[T any] struct {
	Value      T               `json:"value"`
	Evidence   []Evidence      `json:"evidence,omitempty"`
	Confidence float64         `json:"confidence"`
	Status     CandidateStatus `json:"status"`
}

type Diagnostic struct {
	Code     string   `json:"code"`
	Message  string   `json:"message"`
	Location Location `json:"location,omitempty"`
}

type OperationState string

const (
	OperationSourceOnly   OperationState = "source_only"
	OperationDocumentOnly OperationState = "document_only"
	OperationObservedOnly OperationState = "observed_only"
	OperationMatched      OperationState = "matched"
	OperationConflict     OperationState = "conflict"
	OperationUnresolved   OperationState = "unresolved"
	OperationVerified     OperationState = "verified"
)

type Graph struct {
	Operations  map[string]*Operation `json:"operations"`
	Schemas     map[string]*Schema    `json:"schemas"`
	Diagnostics []Diagnostic          `json:"diagnostics,omitempty"`
}

type Operation struct {
	Key         string                            `json:"key"`
	Path        string                            `json:"path"`
	Method      string                            `json:"method"`
	OperationID string                            `json:"operation_id,omitempty"`
	Summary     string                            `json:"summary,omitempty"`
	Description string                            `json:"description,omitempty"`
	Tags        []string                          `json:"tags,omitempty"`
	State       OperationState                    `json:"state"`
	Confidence  float64                           `json:"confidence,omitempty"`
	Gaps        []string                          `json:"gaps,omitempty"`
	Security    []string                          `json:"security,omitempty"`
	Parameters  map[string][]Candidate[Parameter] `json:"parameters,omitempty"`
	RequestBody []Candidate[RequestBody]          `json:"request_body,omitempty"`
	Responses   map[string][]Candidate[Response]  `json:"responses,omitempty"`
	Evidence    []Evidence                        `json:"evidence,omitempty"`
	Diagnostics []Diagnostic                      `json:"diagnostics,omitempty"`
}

type Parameter struct {
	Name        string   `json:"name"`
	In          string   `json:"in"`
	Required    bool     `json:"required"`
	Schema      string   `json:"schema,omitempty"`
	Type        string   `json:"type,omitempty"`
	Format      string   `json:"format,omitempty"`
	Items       string   `json:"items,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Description string   `json:"description,omitempty"`
	Origin      string   `json:"origin,omitempty"`
}

type RequestBody struct {
	ContentType string `json:"content_type"`
	Schema      string `json:"schema,omitempty"`
	Envelope    string `json:"envelope,omitempty"`
	DataField   string `json:"data_field,omitempty"`
	Required    bool   `json:"required"`
}

type Response struct {
	Status       string `json:"status"`
	Description  string `json:"description,omitempty"`
	ContentType  string `json:"content_type,omitempty"`
	Schema       string `json:"schema,omitempty"`
	Envelope     string `json:"envelope,omitempty"`
	DataField    string `json:"data_field,omitempty"`
	MapValueType string `json:"map_value_type,omitempty"`
	Code         int    `json:"code,omitempty"`
	CodeKnown    bool   `json:"code_known,omitempty"`
	HasBody      bool   `json:"has_body,omitempty"`
	Raw          bool   `json:"raw,omitempty"`
	Failure      bool   `json:"failure,omitempty"`
	Sink         string `json:"sink,omitempty"`
	ErrSource    string `json:"err_source,omitempty"`
}

type Schema struct {
	Ref                  string             `json:"ref,omitempty"`
	Types                []string           `json:"types,omitempty"`
	Description          string             `json:"description,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	AdditionalProperties *bool              `json:"additional_properties,omitempty"`
	AllOf                []*Schema          `json:"all_of,omitempty"`
	OneOf                []*Schema          `json:"one_of,omitempty"`
	AnyOf                []*Schema          `json:"any_of,omitempty"`
	Not                  *Schema            `json:"not,omitempty"`
	Nullable             bool               `json:"nullable,omitempty"`
	ContentEncoding      string             `json:"content_encoding,omitempty"`
	ContentMediaType     string             `json:"content_media_type,omitempty"`
	Enum                 []any              `json:"enum,omitempty"`
	Format               string             `json:"format,omitempty"`
	Default              any                `json:"default,omitempty"`
	Examples             []any              `json:"examples,omitempty"`
	Discriminator        string             `json:"discriminator,omitempty"`
	Required             []string           `json:"required,omitempty"`
	ReadOnly             bool               `json:"read_only,omitempty"`
	WriteOnly            bool               `json:"write_only,omitempty"`
	Unknown              bool               `json:"unknown,omitempty"`
	UnknownReason        string             `json:"unknown_reason,omitempty"`
}

func New() *Graph {
	return &Graph{Operations: map[string]*Operation{}, Schemas: map[string]*Schema{}}
}

func OperationKey(method, path string) string {
	return NormalizeMethod(method) + " " + NormalizePath(path)
}

func NormalizeMethod(method string) string { return strings.ToUpper(strings.TrimSpace(method)) }

// NormalizePath keeps operation identity stable across common source/document
// formatting differences without changing parameter semantics.
func NormalizePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return path
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	path = strings.ReplaceAll(path, "//", "/")
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	return path
}

func NormalizeSchemaRef(ref string) string {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "[]")
	ref = strings.TrimPrefix(ref, "#/components/schemas/")
	ref = strings.TrimPrefix(ref, "components/schemas/")
	return ref
}

func (g *Graph) AddOperation(op *Operation) error {
	if g == nil || op == nil {
		return fmt.Errorf("contract graph and operation are required")
	}
	if op.Method == "" || op.Path == "" {
		return fmt.Errorf("operation method and path are required")
	}
	if op.Key == "" {
		op.Key = OperationKey(op.Method, op.Path)
	}
	if g.Operations == nil {
		g.Operations = map[string]*Operation{}
	}
	g.Operations[op.Key] = op
	return nil
}

func (g *Graph) MarshalStable() ([]byte, error) {
	if g == nil {
		return []byte(`{"operations":{},"schemas":{}}`), nil
	}
	normalize(g)
	return json.Marshal(g)
}

func normalize(g *Graph) {
	for _, op := range g.Operations {
		sort.Strings(op.Tags)
		sort.SliceStable(op.Evidence, func(i, j int) bool { return evidenceKey(op.Evidence[i]) < evidenceKey(op.Evidence[j]) })
		for name, candidates := range op.Parameters {
			sort.SliceStable(candidates, func(i, j int) bool { return candidateKey(candidates[i]) < candidateKey(candidates[j]) })
			op.Parameters[name] = candidates
		}
		sort.SliceStable(op.RequestBody, func(i, j int) bool { return candidateKey(op.RequestBody[i]) < candidateKey(op.RequestBody[j]) })
		for status, candidates := range op.Responses {
			sort.SliceStable(candidates, func(i, j int) bool { return candidateKey(candidates[i]) < candidateKey(candidates[j]) })
			op.Responses[status] = candidates
		}
	}
}

func evidenceKey(e Evidence) string {
	return fmt.Sprintf("%s|%s|%d|%d|%s|%s", e.Source, e.Location.File, e.Location.StartLine, e.Location.StartCol, e.Location.JSONPath, e.Summary)
}

func candidateKey[T any](candidate Candidate[T]) string {
	value, _ := json.Marshal(candidate.Value)
	return fmt.Sprintf("%s|%.6f|%s", value, candidate.Confidence, candidate.Status)
}

func (g *Graph) Unmarshal(data []byte) error {
	if err := json.Unmarshal(data, g); err != nil {
		return err
	}
	if g.Operations == nil {
		g.Operations = map[string]*Operation{}
	}
	if g.Schemas == nil {
		g.Schemas = map[string]*Schema{}
	}
	return nil
}
