package eval

import (
	"reflect"
	"testing"
)

func TestAllowOpenObjectExtrasKeepsDeclaredFields(t *testing.T) {
	spec := []string{
		"data|object|true",
		"data.extra|string|false",
		"data.id|integer|true",
	}
	truth := []string{
		"data|object|true",
		"data.id|string|true",
	}
	got := allowOpenObjectExtras(spec, truth, map[string]bool{"data": true})
	want := []string{"data|object|true", "data.id|integer|true"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered fields = %v, want %v", got, want)
	}
}

// TestOneOfResponseFlatten oneOf 响应体（成功信封 + 失败信封同状态码）应展平为各变体字段并集，
// 而非被静默丢弃成空（jsonrpc/jsonrpcmsg 家族 resp_field_f1 曾恒 0）。
func TestOneOfResponseFlatten(t *testing.T) {
	doc := &oasDoc{Paths: map[string]map[string]oasOp{
		"/x": {"post": {Responses: map[string]oasResp{
			"200": {Content: map[string]oasMedia{jsonMediaType: {Schema: oasSchema{OneOf: []oasSchema{
				{Props: map[string]oasSchema{"result": {Ref: "#/components/schemas/Payload"}}},
				{Props: map[string]oasSchema{"error": {Ref: "#/components/schemas/Error"}}},
			}}}}},
		}}},
	}}
	comps := map[string]oasSchema{
		"Payload": {Props: map[string]oasSchema{"id": {Type: "string"}}},
		"Error":   {Props: map[string]oasSchema{"code": {Type: "integer"}}},
	}
	fields := flattenRespData(doc.Paths["/x"]["post"].Responses, comps)
	got := map[string]bool{}
	for _, f := range fields {
		got[f] = true
	}
	for _, want := range []string{"result|object|false", "error|object|false", "result.id|string|false", "error.code|integer|false"} {
		if !got[want] {
			t.Fatalf("oneOf flatten missing %q; got %v", want, fields)
		}
	}
}
