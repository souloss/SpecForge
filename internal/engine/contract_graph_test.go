package engine

import (
	"testing"

	"github.com/specforge/specforge/internal/contract"
	"github.com/specforge/specforge/internal/facts"
)

func TestMergeContractGraphsMarksConflictingOperation(t *testing.T) {
	left, right := contract.New(), contract.New()
	leftOp := &contract.Operation{
		Key: "GET /items", Method: "GET", Path: "/items", State: contract.OperationSourceOnly,
		Responses: map[string][]contract.Candidate[contract.Response]{
			"200": {{Value: contract.Response{Status: "200", Raw: true, HasBody: true, Schema: "A"}, Status: contract.StatusVerified}},
		},
	}
	rightOp := &contract.Operation{
		Key: "GET /items", Method: "GET", Path: "/items", State: contract.OperationDocumentOnly,
		Responses: map[string][]contract.Candidate[contract.Response]{
			"200": {{Value: contract.Response{Status: "200", Raw: true, HasBody: true, Schema: "B"}, Status: contract.StatusDeclared}},
		},
	}
	left.Operations[leftOp.Key] = leftOp
	right.Operations[rightOp.Key] = rightOp

	mergeContractGraphs(left, right)
	if left.Operations[leftOp.Key].State != contract.OperationConflict {
		t.Fatalf("operation state = %q, want conflict", left.Operations[leftOp.Key].State)
	}
	if conflicts, _ := graphStats(left); conflicts == 0 {
		t.Fatal("graphStats reported no conflict")
	}
}

func TestGraphFromFactsKeepsUnresolvedOperationState(t *testing.T) {
	items := []*facts.Fact{{
		ID: "contract:op:GET:/", Kind: facts.KindContract, Source: facts.SourceStatic,
		Confidence: 0.35, Status: "unresolved",
		Value:    facts.ContractPayload{Gaps: []string{"handler unresolved"}, Responses: []facts.ResponseFact{{Status: 200, Raw: true}}},
		Evidence: []facts.Evidence{{File: "router.go", StartLine: 1, Quote: "route registration"}},
	}}
	graph := graphFromFacts(items)
	if graph.Operations["GET /"] == nil || graph.Operations["GET /"].State != contract.OperationUnresolved {
		t.Fatalf("unresolved operation state was lost: %+v", graph.Operations["GET /"])
	}
}
