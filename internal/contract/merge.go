package contract

import "reflect"

// Resolve chooses one candidate only when the values are equivalent. A
// differing set remains visible as a conflict so callers cannot silently lose
// source evidence by changing input order.
func Resolve[T any](candidates []Candidate[T]) (Candidate[T], []Candidate[T]) {
	if len(candidates) == 0 {
		return Candidate[T]{Status: StatusUnresolved}, nil
	}
	resolved := candidates[0]
	for _, candidate := range candidates[1:] {
		if !reflect.DeepEqual(resolved.Value, candidate.Value) {
			for i := range candidates {
				candidates[i].Status = StatusConflict
			}
			return Candidate[T]{Status: StatusConflict}, candidates
		}
		resolved.Evidence = append(resolved.Evidence, candidate.Evidence...)
		if candidate.Confidence > resolved.Confidence {
			resolved.Confidence = candidate.Confidence
		}
	}
	return resolved, candidates
}
