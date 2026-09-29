package memo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFingerprintLLMKeyIsolation 离线与 LLM 键必须不同（防脏缓存）。
func TestFingerprintLLMKeyIsolation(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644)
	off, _ := Fingerprint(dir, "svc", "", "1.0", "offline", isGo)
	llm, _ := Fingerprint(dir, "svc", "", "1.0", "llm:g:x", isGo)
	if off == llm {
		t.Fatalf("offline and llm fingerprints must differ")
	}
}

// TestFingerprintIncludesRepoProfile 未显式指定画像时，仓库内 .specforge/profile.yaml 的变化也会换指纹。
func TestFingerprintIncludesRepoProfile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)
	a, _ := Fingerprint(dir, "", "", "1", "offline", isGo)
	os.MkdirAll(filepath.Join(dir, ".specforge"), 0o755)
	os.WriteFile(filepath.Join(dir, ".specforge", "profile.yaml"), []byte("framework: x\n"), 0o644)
	b, _ := Fingerprint(dir, "", "", "1", "offline", isGo)
	if a == b {
		t.Fatal("editing the repo profile must change the fingerprint")
	}
}

// TestStoreRoundTripAndPrune 存取一致；缺产物视为未命中；超过上限淘汰最旧条目。
func TestStoreRoundTripAndPrune(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Save("fp0", map[string][]byte{"a": []byte("1"), "b": []byte("2")}); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Load("fp0", "a", "b")
	if !ok || string(got["a"]) != "1" || string(got["b"]) != "2" {
		t.Fatalf("round trip failed: %v %v", got, ok)
	}
	if _, ok := s.Load("fp0", "a", "missing"); ok {
		t.Fatal("missing artifact must be a miss")
	}
	for i := 1; i <= MaxEntries+2; i++ {
		s.Save("fp"+string(rune('a'+i)), map[string][]byte{"a": nil})
	}
	ents, _ := os.ReadDir(s.dir)
	if len(ents) != MaxEntries {
		t.Fatalf("entries = %d, want %d", len(ents), MaxEntries)
	}
}

// TestStoreDisabled 空目录禁用缓存。
func TestStoreDisabled(t *testing.T) {
	s := New("")
	if err := s.Save("x", map[string][]byte{"a": nil}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Load("x", "a"); ok {
		t.Fatal("disabled store must never hit")
	}
}

// isGo 测试用源码判定（Go 前端口径）。
func isGo(name string) bool {
	return strings.HasSuffix(name, ".go") || name == "go.mod" || name == "go.sum"
}
