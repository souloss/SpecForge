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
