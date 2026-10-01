package contract

import "sort"

// GraphChange describes a source-independent change between two contract
// graphs. It intentionally does not reuse OpenAPI document diff semantics.
type GraphChange struct {
	Key      string `json:"key"`
	Kind     string `json:"kind"`
	Breaking bool   `json:"breaking"`
}

type GraphDiff struct {
	Changes []GraphChange `json:"changes,omitempty"`
}

// Diff compares operation presence and states. Field-level candidate changes
// remain available in the graph and are intentionally not flattened here.
func Diff(left, right *Graph) *GraphDiff {
	result := &GraphDiff{}
	if left == nil {
		left = New()
	}
	if right == nil {
		right = New()
	}
	for key := range left.Operations {
		if _, ok := right.Operations[key]; !ok {
			result.Changes = append(result.Changes, GraphChange{Key: key, Kind: "operation_removed", Breaking: true})
		}
	}
	for key, operation := range right.Operations {
		previous, ok := left.Operations[key]
		if !ok {
			result.Changes = append(result.Changes, GraphChange{Key: key, Kind: "operation_added"})
			continue
		}
		if previous.State != operation.State {
			breaking := operation.State == OperationConflict || operation.State == OperationUnresolved
			result.Changes = append(result.Changes, GraphChange{Key: key, Kind: "operation_state_changed", Breaking: breaking})
		}
	}
	sort.Slice(result.Changes, func(i, j int) bool {
		if result.Changes[i].Key != result.Changes[j].Key {
			return result.Changes[i].Key < result.Changes[j].Key
		}
		return result.Changes[i].Kind < result.Changes[j].Kind
	})
	return result
}
