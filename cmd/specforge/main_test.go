package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// sampleRepo 样本仓库路径。
var sampleRepo = filepath.Join("..", "..", "testdata", "sample-repo")

// runCLI 执行 CLI，返回退出码与 stdout/stderr。
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

// decodeEnvelope 断言 stdout 恰好是一个 JSON 信封并解码。
func decodeEnvelope(t *testing.T, out string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	var env map[string]any
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if dec.More() {
		t.Fatalf("stdout must contain exactly one JSON object:\n%s", out)
	}
	if env["schema_version"] != "1" {
		t.Fatalf("missing schema_version: %v", env)
	}
	return env
}

// genSample 生成样本仓库产物到临时目录。
func genSample(t *testing.T) string {
	t.Helper()
	outDir := t.TempDir()
	if code, _, errs := runCLI(t, "gen", "--repo", sampleRepo, "--no-cache", "-o", outDir, "-q"); code != exitOK {
		t.Fatalf("gen exit %d: %s", code, errs)
	}
	return outDir
}

// TestExitCodes 退出码契约：每类错误对应的退出码固定。
func TestExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"unknown flag", []string{"gen", "--nope"}, exitUsage},
		{"unknown command", []string{"frobnicate"}, exitUsage},
		{"bad log level", []string{"gen", "--repo", sampleRepo, "--log-level", "loud"}, exitUsage},
		{"no go.mod", []string{"gen", "--repo", t.TempDir(), "--no-cache"}, exitConfig},
		{"eval missing args", []string{"eval"}, exitUsage},
		{"unknown frontend", []string{"gen", "--repo", sampleRepo, "--frontend", "cobol", "--no-cache"}, exitUsage},
		{"generic frontend offline", []string{"gen", "--repo", filepath.Join("..", "..", "testdata", "express-repo"), "--no-cache", "-o", t.TempDir()}, exitConfig},
		{"explain without gen", []string{"explain", "GET", "/x", "--repo", t.TempDir()}, exitConfig},
		{"ops without gen", []string{"ops", "--repo", t.TempDir()}, exitConfig},
		{"doctor on non-module", []string{"doctor", "--repo", t.TempDir()}, exitConfig},
	}
	for _, c := range cases {
		if got, _, _ := runCLI(t, c.args...); got != c.want {
			t.Errorf("%s: exit %d, want %d", c.name, got, c.want)
		}
	}
}

// TestErrorEnvelope 失败时 --json 输出 ok=false 与 error.code/exit_code。
func TestErrorEnvelope(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	code, out, _ := runCLI(t, "gen", "--repo", sampleRepo, "--llm", "--json", "--no-cache", "-o", t.TempDir())
	env := decodeEnvelope(t, out)
	e, _ := env["error"].(map[string]any)
	if code != exitConfig || env["ok"] != false || e["code"] != codeLLMNotConfig || e["exit_code"].(float64) != exitConfig {
		t.Fatalf("exit=%d env=%v", code, env)
	}
	if env["command"] != "gen" {
		t.Fatalf("command = %v", env["command"])
	}
}

// TestGenOpsExplainJSON gen/ops/explain 的 --json 都是单信封，字段为 snake_case。
func TestGenOpsExplainJSON(t *testing.T) {
	outDir := t.TempDir()
	code, out, _ := runCLI(t, "gen", "--repo", sampleRepo, "--json", "--no-cache", "-o", outDir)
	env := decodeEnvelope(t, out)
	if code != exitOK || env["ok"] != true || env["data"].(map[string]any)["operations"].(float64) != 10 {
		t.Fatalf("gen exit %d env %v", code, env)
	}
	code, out, _ = runCLI(t, "ops", "-o", outDir, "--json")
	env = decodeEnvelope(t, out)
	if rows, ok := env["data"].([]any); code != exitOK || !ok || len(rows) != 10 {
		t.Fatalf("ops exit %d env %v", code, env)
	}
	code, out, _ = runCLI(t, "explain", "post", "/ipo/v1/OrderCheck", "-o", outDir, "--json")
	env = decodeEnvelope(t, out)
	op, _ := env["data"].(map[string]any)
	if code != exitOK || op["operation_id"] != "orderCheck" || op["responses"] == nil {
		t.Fatalf("explain exit %d env %v", code, env)
	}
	code, _, errs := runCLI(t, "explain", "POST", "/ipo/v9/OrderCheck", "-o", outDir)
	if code != exitConfig || !strings.Contains(errs, "did you mean") {
		t.Fatalf("explain miss should suggest alternatives: exit %d, %s", code, errs)
	}
}

// TestOpsFilters --low-confidence / --gaps 只保留对应 operation。
func TestOpsFilters(t *testing.T) {
	outDir := genSample(t)
	_, all, _ := runCLI(t, "ops", "-o", outDir)
	_, low, _ := runCLI(t, "ops", "-o", outDir, "--low-confidence")
	if !strings.Contains(all, "10 of 10 operations") || strings.Contains(low, "10 of 10") {
		t.Fatalf("filters not applied:\n%s\n%s", all, low)
	}
}

// TestEvalGate --fail-under 未达标返回 40，且 --json 仍只输出一个信封（含指标与闸门结论）。
func TestEvalGate(t *testing.T) {
	outDir := genSample(t)
	truth := filepath.Join("..", "..", "testdata", "ground-truth.yaml")
	spec := filepath.Join(outDir, "openapi.yaml")
	if code, _, _ := runCLI(t, "eval", "--truth", truth, "--spec", spec, "--fail-under", "0.99"); code != exitOK {
		t.Fatalf("perfect spec should pass the gate, exit %d", code)
	}
	empty := filepath.Join(t.TempDir(), "empty.yaml")
	os.WriteFile(empty, []byte("openapi: 3.1.0\npaths: {}\n"), 0o644)
	code, out, _ := runCLI(t, "eval", "--truth", truth, "--spec", empty, "--fail-under", "0.9", "--json")
	env := decodeEnvelope(t, out)
	data, _ := env["data"].(map[string]any)
	if code != exitGateFailed || env["ok"] != false || data["gate_pass"] != false || data["metrics"] == nil {
		t.Fatalf("exit %d env %v", code, env)
	}
}

// TestHelpIsDocumented 每个可执行命令都有详细说明与示例（帮助是用户契约的一部分）。
func TestHelpIsDocumented(t *testing.T) {
	root := newRootCmd(&app{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}})
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sc := range c.Commands() {
			if sc.Hidden || sc.Name() == "help" || sc.Name() == "completion" {
				continue
			}
			if sc.Runnable() && sc.Example == "" {
				t.Errorf("%s: missing Example", sc.CommandPath())
			}
			if sc.Parent() == root && sc.Long == "" {
				t.Errorf("%s: missing Long description", sc.CommandPath())
			}
			walk(sc)
		}
	}
	walk(root)
}
