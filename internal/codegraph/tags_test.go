package codegraph

import "testing"

func TestTagValueOf(t *testing.T) {
	tag := "`json:\"orderId,omitempty\" binding:\"required,min=1,max=64\"`"
	if got := tagValueOf(tag, "json"); got != "orderId,omitempty" {
		t.Errorf("json tag = %q", got)
	}
	if got := tagValueOf(tag, "binding"); got != "required,min=1,max=64" {
		t.Errorf("binding tag = %q", got)
	}
	if got := tagValueOf(tag, "yaml"); got != "" {
		t.Errorf("missing key should be empty, got %q", got)
	}
}

func TestJSONNameOf(t *testing.T) {
	cases := []struct {
		field, tag, want string
	}{
		{"OrderID", "`json:\"orderId\"`", "orderId"},
		{"Foo", "`json:\"foo,omitempty\"`", "foo"},
		{"Bar", "`json:\"-\"`", "-"},
		{"Baz", "", "Baz"}, // 无 tag → 字段名
	}
	for _, c := range cases {
		if got := jsonNameOf(c.field, c.tag); got != c.want {
			t.Errorf("jsonNameOf(%q, %q) = %q, want %q", c.field, c.tag, got, c.want)
		}
	}
}

func TestHasOmitempty(t *testing.T) {
	if !hasOmitempty("`json:\"fee,omitempty\"`") {
		t.Error("omitempty should be true")
	}
	if hasOmitempty("`json:\"fee\"`") {
		t.Error("plain tag should be false")
	}
	if hasOmitempty("") {
		t.Error("empty tag should be false")
	}
}
