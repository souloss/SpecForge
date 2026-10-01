package openapi

import (
	"fmt"
	"sort"

	"github.com/pb33f/libopenapi/what-changed"
	changeModel "github.com/pb33f/libopenapi/what-changed/model"
)

type Change struct {
	Property string `json:"property,omitempty"`
	Path     string `json:"path,omitempty"`
	Kind     string `json:"kind"`
	Breaking bool   `json:"breaking"`
	Original string `json:"original,omitempty"`
	New      string `json:"new,omitempty"`
}

type DocumentDiff struct {
	Changes []Change `json:"changes,omitempty"`
}

func CompareDocuments(leftPath, rightPath string) (*DocumentDiff, error) {
	left, err := Load(leftPath)
	if err != nil {
		return nil, err
	}
	defer left.Close()
	right, err := Load(rightPath)
	if err != nil {
		return nil, err
	}
	defer right.Close()
	leftModel, err := left.document.BuildV3Model()
	if err != nil {
		return nil, fmt.Errorf("build left OpenAPI model: %w", err)
	}
	rightModel, err := right.document.BuildV3Model()
	if err != nil {
		return nil, fmt.Errorf("build right OpenAPI model: %w", err)
	}
	changes := what_changed.CompareOpenAPIDocuments(leftModel.Model.GoLow(), rightModel.Model.GoLow())
	result := &DocumentDiff{}
	for _, change := range changes.GetAllChanges() {
		if change == nil {
			continue
		}
		kind := changeKind(change.ChangeType)
		result.Changes = append(result.Changes, Change{Property: change.Property, Path: change.Path, Kind: kind, Breaking: change.Breaking, Original: change.Original, New: change.New})
	}
	sort.SliceStable(result.Changes, func(i, j int) bool {
		if result.Changes[i].Path != result.Changes[j].Path {
			return result.Changes[i].Path < result.Changes[j].Path
		}
		return result.Changes[i].Kind < result.Changes[j].Kind
	})
	return result, nil
}

func changeKind(kind int) string {
	switch kind {
	case changeModel.Modified:
		return "modified"
	case changeModel.PropertyAdded:
		return "property_added"
	case changeModel.ObjectAdded:
		return "object_added"
	case changeModel.ObjectRemoved:
		return "object_removed"
	case changeModel.PropertyRemoved:
		return "property_removed"
	default:
		return fmt.Sprintf("change-%d", kind)
	}
}
