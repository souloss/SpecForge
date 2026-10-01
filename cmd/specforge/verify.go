package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/specforge/specforge/internal/openapi"
)

type verifyResult struct {
	Spec        string               `json:"spec"`
	Valid       bool                 `json:"valid"`
	Diagnostics []openapi.Diagnostic `json:"diagnostics,omitempty"`
}

func newVerifyCmd(a *app) *cobra.Command {
	var spec string
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Load and validate an OpenAPI document",
		Long: `Load an OpenAPI 3.0, 3.1, or 3.2 document, resolve local references, and validate it
against the OpenAPI specification. Diagnostics include the validation kind and source location.
The command exits with code 30 when the document cannot be loaded or fails validation.`,
		Example: `  specforge verify --spec .specforge/out/openapi.yaml
  specforge verify --spec api/openapi.json --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if spec == "" {
				return newErr(exitUsage, codeInvalidArgs, "--spec is required", "see 'specforge verify --help'")
			}
			doc, err := openapi.Load(spec)
			if err != nil {
				return newErr(exitConfig, codeConfigInvalid, err.Error(), "check the OpenAPI version, references, and YAML syntax")
			}
			defer doc.Close()
			result := verifyResult{Spec: spec, Valid: true}
			result.Diagnostics = doc.Validate()
			result.Valid = len(result.Diagnostics) == 0
			a.emit(result, func(w io.Writer) {
				if result.Valid {
					fmt.Fprintf(w, "valid: %s\n", spec)
					return
				}
				fmt.Fprintf(w, "invalid: %s\n", spec)
				for _, diagnostic := range result.Diagnostics {
					fmt.Fprintf(w, "  %s: %s\n", diagnostic.Code, diagnostic.Message)
				}
			})
			if !result.Valid {
				return newErr(exitConfig, codeConfigInvalid, "OpenAPI document failed validation", "fix the reported diagnostics")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&spec, "spec", "", "OpenAPI YAML or JSON file to validate (required)")
	return cmd
}
