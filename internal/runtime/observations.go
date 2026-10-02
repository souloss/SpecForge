package runtime

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/specforge/specforge/internal/contract"
)

// Observation is the file-ingestion form of a redacted HTTP exchange. Bodies
// are intentionally absent: runtime input can prove an operation/status/media
// type was observed, but it cannot invent a response schema.
type Observation struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type,omitempty"`
	TraceID     string `json:"trace_id,omitempty"`
	ObservedAt  string `json:"observed_at,omitempty"`
}

// ImportObservations reads one JSON object per line and returns observed
// Contract Graph candidates. Blank lines are ignored; malformed lines fail the
// whole file so CI cannot silently lose runtime coverage.
func ImportObservations(path string) (*contract.Graph, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	graph := contract.New()
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		data := strings.TrimSpace(scanner.Text())
		if data == "" {
			continue
		}
		var observation Observation
		if err := json.Unmarshal([]byte(data), &observation); err != nil {
			return nil, fmt.Errorf("%s:%d: decode runtime observation: %w", path, line, err)
		}
		if observation.Method == "" || observation.Path == "" || observation.Status < 100 || observation.Status > 599 {
			return nil, fmt.Errorf("%s:%d: method, path and status 100..599 are required", path, line)
		}
		method := contract.NormalizeMethod(observation.Method)
		path := contract.NormalizePath(observation.Path)
		key := contract.OperationKey(method, path)
		op := graph.Operations[key]
		if op == nil {
			op = &contract.Operation{Key: key, Method: method, Path: path, State: contract.OperationObservedOnly, Confidence: 0.85, Parameters: map[string][]contract.Candidate[contract.Parameter]{}, Responses: map[string][]contract.Candidate[contract.Response]{}}
			graph.Operations[key] = op
		}
		status := fmt.Sprintf("%d", observation.Status)
		candidate := contract.Candidate[contract.Response]{
			Value:      contract.Response{Status: status, ContentType: observation.ContentType, HasBody: observation.ContentType != "", Raw: observation.ContentType != ""},
			Confidence: 0.85,
			Status:     contract.StatusObserved,
			Evidence:   []contract.Evidence{{Source: contract.SourceRuntime, Location: contract.Location{File: path, StartLine: line, TraceID: observation.TraceID, ObservedAt: observation.ObservedAt}, Strength: contract.StrengthDirect}},
		}
		op.Evidence = append(op.Evidence, candidate.Evidence...)
		op.Responses[status] = append(op.Responses[status], candidate)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read runtime observations %q: %w", path, err)
	}
	return graph, nil
}
