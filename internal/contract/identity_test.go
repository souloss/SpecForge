package contract

import "testing"

func TestOperationKeyNormalizesMethodAndPath(t *testing.T) {
	if got, want := OperationKey(" get ", "/users/"), "GET /users"; got != want {
		t.Fatalf("OperationKey() = %q, want %q", got, want)
	}
	if OperationKey("GET", "/users") != OperationKey("get", "users/") {
		t.Fatal("equivalent operation identities did not normalize to the same key")
	}
}
