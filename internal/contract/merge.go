package contract

import (
	"encoding/json"
	"reflect"
	"sort"
)

// Resolve chooses one candidate only when the values are equivalent. A
// differing set remains visible as a conflict so callers cannot silently lose
// source evidence by changing input order.
func Resolve[T any](candidates []Candidate[T]) (Candidate[T], []Candidate[T]) {
	if len(candidates) == 0 {
		return Candidate[T]{Status: StatusUnresolved}, nil
	}
	retained := append([]Candidate[T](nil), candidates...)
	sort.SliceStable(retained, func(i, j int) bool {
		return candidatePriority(retained[i]) > candidatePriority(retained[j])
	})
	resolved := retained[0]
	for _, candidate := range retained[1:] {
		if !equivalent(resolved.Value, candidate.Value) {
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
	resolved.Status = strongestStatus(retained)
	sort.SliceStable(resolved.Evidence, func(i, j int) bool { return evidenceKey(resolved.Evidence[i]) < evidenceKey(resolved.Evidence[j]) })
	return resolved, retained
}

func equivalent[T any](left, right T) bool {
	if reflect.DeepEqual(left, right) {
		return true
	}
	// JSON comparison treats semantically identical values consistently even
	// when callers supplied different concrete map types.
	lb, lerr := json.Marshal(left)
	rb, rerr := json.Marshal(right)
	return lerr == nil && rerr == nil && string(lb) == string(rb)
}

func candidatePriority[T any](candidate Candidate[T]) float64 {
	status := map[CandidateStatus]float64{
		StatusVerified:     5,
		StatusDeclared:     4,
		StatusObserved:     3,
		StatusInferred:     2,
		StatusInconclusive: 1,
		StatusUnresolved:   0,
	}
	return status[candidate.Status] + candidate.Confidence
}

func strongestStatus[T any](candidates []Candidate[T]) CandidateStatus {
	if len(candidates) == 0 {
		return StatusUnresolved
	}
	best := candidates[0].Status
	bestScore := candidatePriority(candidates[0]) - candidates[0].Confidence
	for _, candidate := range candidates[1:] {
		score := candidatePriority(candidate) - candidate.Confidence
		if score > bestScore {
			best, bestScore = candidate.Status, score
		}
	}
	return best
}
