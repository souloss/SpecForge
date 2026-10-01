package openapi

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pb33f/libopenapi"
)

type OverlayWarning struct {
	Target  string `json:"target,omitempty"`
	Message string `json:"message"`
}

type OverlayResult struct {
	Bytes    []byte           `json:"-"`
	Warnings []OverlayWarning `json:"warnings,omitempty"`
	Document *Document        `json:"-"`
}

// ApplyOverlay validates the base, applies an OpenAPI Overlay, then validates
// and reloads the resulting document. It never mutates the base Document.
func ApplyOverlay(basePath, overlayPath string) (*OverlayResult, error) {
	base, err := Load(basePath)
	if err != nil {
		return nil, err
	}
	defer base.Close()
	if diagnostics := base.Validate(); len(diagnostics) > 0 {
		return nil, fmt.Errorf("base OpenAPI validation failed: %s", formatDiagnostics(diagnostics))
	}
	overlay, err := os.ReadFile(overlayPath)
	if err != nil {
		return nil, fmt.Errorf("read overlay %q: %w", overlayPath, err)
	}
	result, err := libopenapi.ApplyOverlayFromBytes(base.document, overlay)
	if err != nil {
		return nil, fmt.Errorf("apply overlay %q: %w", overlayPath, err)
	}
	resultDoc, err := LoadBytes(result.Bytes, filepath.Join(filepath.Dir(basePath), ".overlay-result.yaml"))
	if err != nil {
		return nil, fmt.Errorf("reload overlay result: %w", err)
	}
	if diagnostics := resultDoc.Validate(); len(diagnostics) > 0 {
		resultDoc.Close()
		return nil, fmt.Errorf("overlay result validation failed: %s", formatDiagnostics(diagnostics))
	}
	warnings := make([]OverlayWarning, 0, len(result.Warnings))
	for _, warning := range result.Warnings {
		if warning != nil {
			warnings = append(warnings, OverlayWarning{Target: warning.Target, Message: warning.Message})
		}
	}
	return &OverlayResult{Bytes: result.Bytes, Warnings: warnings, Document: resultDoc}, nil
}

func formatDiagnostics(diagnostics []Diagnostic) string {
	if len(diagnostics) == 0 {
		return ""
	}
	return diagnostics[0].Code + ": " + diagnostics[0].Message
}
