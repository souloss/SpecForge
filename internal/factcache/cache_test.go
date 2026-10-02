package factcache

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/specforge/specforge/internal/contract"
)

func TestCacheRoundTripAndVersionIsolation(t *testing.T) {
	cache, err := Open(filepath.Join(t.TempDir(), "facts.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "routes.go"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	graph := contract.New()
	graph.Operations["GET /items"] = &contract.Operation{Key: "GET /items", Method: "GET", Path: "/items", State: contract.OperationSourceOnly, Evidence: []contract.Evidence{{Source: contract.SourceCode, Location: contract.Location{File: "routes.go"}}}}
	if err := cache.SaveWithRoot("fp", "engine-1", graph, root); err != nil {
		t.Fatal(err)
	}
	loaded, hit, err := cache.Load("fp", "engine-1")
	if err != nil || !hit || loaded.Operations["GET /items"] == nil {
		t.Fatalf("load = graph=%v hit=%v err=%v", loaded, hit, err)
	}
	if _, hit, err := cache.Load("fp", "engine-2"); err != nil || hit {
		t.Fatalf("version isolation failed: hit=%v err=%v", hit, err)
	}
	hash, ok := contentHash(filepath.Join(root, "routes.go"))
	if !ok {
		t.Fatal("content hash unavailable")
	}
	if stale, err := cache.StaleOperations("fp", "engine-1", map[string]string{"routes.go": hash}); err != nil || len(stale) != 0 {
		t.Fatalf("unchanged dependency marked stale: %v, err=%v", stale, err)
	}
	stale, err := cache.StaleOperations("fp", "engine-1", map[string]string{"routes.go": "changed"})
	if err != nil || len(stale) != 1 || stale[0] != "GET /items" {
		t.Fatalf("stale operations = %v, err=%v", stale, err)
	}
}
