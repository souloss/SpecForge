package runtime

import (
	"net/http"
	"testing"
)

func TestRedactSamplesExcludePayloadAndCredentials(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.test/users", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	sample := RedactRequest(req)
	if sample.Path != "/users" || sample.ContentType != "application/json" || sample.BodyBytes != 0 {
		t.Fatalf("unexpected request sample: %+v", sample)
	}
	resp := RedactResponse(&http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}})
	if resp.Status != http.StatusBadRequest || resp.ContentType != "application/json" || resp.BodyBytes != 0 {
		t.Fatalf("unexpected response sample: %+v", resp)
	}
}
