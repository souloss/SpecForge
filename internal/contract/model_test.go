package contract

import "testing"

func TestGraphRetainsCandidatesAndRoundTrips(t *testing.T) {
	g := New()
	op := &Operation{
		Method: "GET", Path: "/pets/{id}", State: OperationConflict,
		Parameters: map[string][]Candidate[Parameter]{"id": {
			{Value: Parameter{Name: "id", In: "path", Required: true}, Status: StatusVerified, Confidence: 1, Evidence: []Evidence{{Source: SourceCode, Location: Location{File: "routes.go", StartLine: 12}, Strength: StrengthDirect}}},
			{Value: Parameter{Name: "id", In: "path", Required: false}, Status: StatusDeclared, Confidence: .8, Evidence: []Evidence{{Source: SourceOpenAPI, Location: Location{Document: "openapi.yaml", JSONPath: "$.paths['/pets/{id}'].get.parameters[0]"}, Strength: StrengthDirect}}},
		}},
	}
	if err := g.AddOperation(op); err != nil {
		t.Fatal(err)
	}
	data, err := g.MarshalStable()
	if err != nil {
		t.Fatal(err)
	}
	var got Graph
	if err := got.Unmarshal(data); err != nil {
		t.Fatal(err)
	}
	if len(got.Operations["GET /pets/{id}"].Parameters["id"]) != 2 {
		t.Fatal("candidate evidence was lost during round trip")
	}
}

func TestResolveMarksConflictsWithoutOrderWinner(t *testing.T) {
	candidates := []Candidate[string]{
		{Value: "string", Status: StatusVerified, Confidence: 1},
		{Value: "integer", Status: StatusDeclared, Confidence: .8},
	}
	resolved, retained := Resolve(candidates)
	if resolved.Status != StatusConflict || len(retained) != 2 {
		t.Fatalf("resolved=%+v retained=%+v", resolved, retained)
	}
	for _, candidate := range retained {
		if candidate.Status != StatusConflict {
			t.Fatalf("candidate status = %q", candidate.Status)
		}
	}
}

func TestUnknownSchemaIsExplicit(t *testing.T) {
	g := New()
	g.Schemas["Upload"] = &Schema{Unknown: true, UnknownReason: "generic map could not be resolved"}
	data, err := g.MarshalStable()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || !contains(string(data), "unknown_reason") {
		t.Fatalf("unknown schema was not serialized: %s", data)
	}
}

func contains(s, want string) bool {
	for i := 0; i+len(want) <= len(s); i++ {
		if s[i:i+len(want)] == want {
			return true
		}
	}
	return false
}
