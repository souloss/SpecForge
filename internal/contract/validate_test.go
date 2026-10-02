package contract

import "testing"

func TestValidateGraphFindsMissingPathParameterAndUnknownSchema(t *testing.T) {
	g := New()
	if err := g.AddOperation(&Operation{
		Key: "GET /pets/{id}", Method: "GET", Path: "/pets/{id}", State: OperationSourceOnly,
		Parameters: map[string][]Candidate[Parameter]{},
		Responses:  map[string][]Candidate[Response]{"200": {{Value: Response{Status: "200", Schema: "Pet"}, Status: StatusVerified}}},
	}); err != nil {
		t.Fatal(err)
	}
	g.Schemas["Pet"] = &Schema{Unknown: true, UnknownReason: "dynamic type"}
	diagnostics := ValidateGraph(g)
	if len(diagnostics) != 2 {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
}

func TestValidateGraphFindsResponseConflict(t *testing.T) {
	g := New()
	g.Schemas["Pet"] = &Schema{}
	g.Schemas["Other"] = &Schema{}
	if err := g.AddOperation(&Operation{
		Key: "GET /pets", Method: "GET", Path: "/pets", Parameters: map[string][]Candidate[Parameter]{},
		Responses: map[string][]Candidate[Response]{"200": {
			{Value: Response{Status: "200", Schema: "Pet"}, Status: StatusVerified},
			{Value: Response{Status: "200", Schema: "Other"}, Status: StatusDeclared},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	diagnostics := ValidateGraph(g)
	if len(diagnostics) != 1 || diagnostics[0].Code != "response_conflict" {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
}

func TestValidateGraphRetainsSourceMergeDiagnostics(t *testing.T) {
	g := New()
	g.Diagnostics = append(g.Diagnostics, Diagnostic{Code: "schema_conflict", Message: `schema "Result" differs between sources`})

	diagnostics := ValidateGraph(g)
	if len(diagnostics) != 1 || diagnostics[0].Code != "schema_conflict" {
		t.Fatalf("ValidateGraph() = %#v, want source merge diagnostic", diagnostics)
	}
}

func TestValidateGraphFindsMissingSchemaReference(t *testing.T) {
	g := New()
	g.Operations["GET /items"] = &Operation{
		Key: "GET /items", Method: "GET", Path: "/items",
		Responses: map[string][]Candidate[Response]{"200": {{Value: Response{Status: "200", Schema: "Missing"}, Status: StatusVerified}}},
	}
	for _, diagnostic := range ValidateGraph(g) {
		if diagnostic.Code == "schema_reference_missing" {
			return
		}
	}
	t.Fatal("missing schema reference was not diagnosed")
}
