package typeschema

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/specforge/specforge/internal/codegraph"
	"github.com/specforge/specforge/internal/loader"
)

func TestSynthesizeOpaqueUnionIncludesAlternatives(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module unionfixture\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "union.go"), []byte(`package unionfixture
import "encoding/json"
type Kinds struct { union json.RawMessage }
type Kinds0 string
type KindID string
type Kinds1 = []KindID
func (Kinds) AsKinds0() (Kinds0, error) { return "", nil }
func (Kinds) AsKinds1() (Kinds1, error) { return nil, nil }
func (Kinds) MarshalJSON() ([]byte, error) { return nil, nil }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loader.LoadRepo(root)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := codegraph.Build(loaded.Pkgs, loaded.Fset)
	if err != nil {
		t.Fatal(err)
	}
	schema := New(graph).Synthesize("unionfixture.Kinds")
	if len(schema.OneOf) != 2 {
		t.Fatalf("union schema = %+v", schema)
	}
}
