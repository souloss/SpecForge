package contract

import "testing"

func TestResolveUsesSourcePriorityForEquivalentCandidates(t *testing.T) {
	code := Candidate[string]{Value: "same", Status: StatusVerified, Confidence: .7, Evidence: []Evidence{{Source: SourceCode}}}
	document := Candidate[string]{Value: "same", Status: StatusDeclared, Confidence: 1, Evidence: []Evidence{{Source: SourceOpenAPI}}}
	resolved, _ := Resolve([]Candidate[string]{document, code})
	if resolved.Status != StatusVerified || resolved.Confidence != 1 {
		t.Fatalf("resolved = %#v, want verified representative with merged confidence", resolved)
	}
}

func TestResolveKeepsDifferentValuesAsConflict(t *testing.T) {
	resolved, retained := Resolve([]Candidate[string]{
		{Value: "code", Status: StatusVerified, Evidence: []Evidence{{Source: SourceCode}}},
		{Value: "runtime", Status: StatusObserved, Evidence: []Evidence{{Source: SourceRuntime}}},
	})
	if resolved.Status != StatusConflict || len(retained) != 2 {
		t.Fatalf("resolved=%#v retained=%d, want conflict with both candidates", resolved, len(retained))
	}
	for _, candidate := range retained {
		if candidate.Status != StatusConflict {
			t.Fatalf("candidate status = %q, want conflict", candidate.Status)
		}
	}
}
