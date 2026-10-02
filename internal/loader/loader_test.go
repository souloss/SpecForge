package loader

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestLoadRepoContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	repo := filepath.Join("..", "..", "testdata", "sample-repo")
	if _, err := LoadRepoContext(ctx, repo); !errors.Is(err, context.Canceled) {
		t.Fatalf("LoadRepoContext error = %v, want context.Canceled", err)
	}
}
