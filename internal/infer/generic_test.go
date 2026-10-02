package infer

import (
	"context"
	"encoding/json"
	"testing"
)

type genericTaskProvider struct{ request Request }

func (p *genericTaskProvider) Name() string { return "generic-task-test" }
func (p *genericTaskProvider) Complete(_ context.Context, req Request) (Response, error) {
	p.request = req
	return Response{Text: []byte(`{"params":[],"requestBody":null,"responses":[]}`)}, nil
}

func TestExtractContractCarriesUnifiedMetadata(t *testing.T) {
	p := &genericTaskProvider{}
	_, err := ExtractContract(context.Background(), p, ContractTask{
		Source: "generic", Operation: "GET /items", AllowedFields: []string{"params", "responses"},
		Method: "GET", Path: "/items",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var task ContractTask
	if err := json.Unmarshal([]byte(p.request.Prompt), &task); err != nil {
		t.Fatal(err)
	}
	if task.Source != "generic" || task.Operation != "GET /items" || len(task.AllowedFields) != 2 {
		t.Fatalf("unified metadata missing: %+v", task)
	}
}
