package memo

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFingerprintLLMKeyIsolation 离线与 LLM 键必须不同（防脏缓存）。
func TestFingerprintLLMKeyIsolation(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644)
	off, _ := Fingerprint(dir, "svc", "", "1.0", "offline")
	llm, _ := Fingerprint(dir, "svc", "", "1.0", "llm:g:x")
	if off == llm {
		t.Fatalf("offline and llm fingerprints must differ")
	}
}
