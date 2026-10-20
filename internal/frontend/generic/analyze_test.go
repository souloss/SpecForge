package generic

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/specforge/specforge/internal/infer"
)

// TestResolveAllMultipleCandidates 同名方法歧义时 Resolve 拒绝、ResolveAll 返回全部候选（按 ID 有序）。
func TestResolveAllMultipleCandidates(t *testing.T) {
	root := t.TempDir()
	// 两个同名方法分处不同文件（模拟 controller/service 分层），注册文件为第三个文件。
	if err := os.WriteFile(filepath.Join(root, "controller.go"), []byte("package routes\ntype Controller struct{}\nfunc (c *Controller) CaseDetail() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "service.go"), []byte("package routes\ntype Service struct{}\nfunc (s *Service) CaseDetail() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "router.go"), []byte("package routes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prog, err := NewProgram(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := prog.Resolve("CaseDetail", filepath.Join(root, "router.go")); ok {
		t.Fatal("Resolve should reject ambiguous name")
	}
	all := prog.ResolveAll("CaseDetail")
	if len(all) != 2 {
		t.Fatalf("ResolveAll = %d candidates, want 2", len(all))
	}
	if all[0].ID >= all[1].ID {
		t.Fatalf("ResolveAll not sorted by ID: %v", all)
	}
}

// TestNormalizePath 各框架路径参数写法归一为 OpenAPI {name}。
func TestNormalizePath(t *testing.T) {
	cases := map[string]struct {
		path   string
		params []string
	}{
		"/users/:id":                 {"/users/{id}", []string{"id"}},
		"/users/<int:uid>/posts/<p>": {"/users/{uid}/posts/{p}", []string{"uid", "p"}},
		"/items/{id:[0-9]+}/":        {"/items/{id}", []string{"id"}},
		"/":                          {"/", nil},
	}
	for in, want := range cases {
		got, params := normalizePath(in)
		if got != want.path || !reflect.DeepEqual(params, want.params) {
			t.Errorf("normalizePath(%q) = %q %v, want %q %v", in, got, params, want.path, want.params)
		}
	}
}

func TestVerifyRouteAllowsTraceButNotConnect(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "routes.js")
	if err := os.WriteFile(file, []byte("router.trace('/events', traceEvents)\nfunction traceEvents() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prog, err := NewProgram(root)
	if err != nil {
		t.Fatal(err)
	}
	trace, err := verifyRoute(prog, file, infer.DiscoveredRoute{Method: "TRACE", Path: "/events", Handler: "traceEvents", Line: 1})
	if err != nil || trace.method != "TRACE" {
		t.Fatalf("TRACE route = %+v, error = %v", trace, err)
	}
	if _, err := verifyRoute(prog, file, infer.DiscoveredRoute{Method: "CONNECT", Path: "/events", Handler: "traceEvents", Line: 1}); err == nil {
		t.Fatal("CONNECT cannot be represented as an OpenAPI Path Item operation")
	}
}

// TestPathLiteralIn 注册行上的字面量须等于所报路径或按段边界为其后缀。
func TestPathLiteralIn(t *testing.T) {
	window := `router.get('/users/:id', getUser)`
	if lit, ok := pathLiteralIn(window, "/api/users/:id"); !ok || lit != "/users/:id" {
		t.Fatalf("suffix literal not found: %q %v", lit, ok)
	}
	if _, ok := pathLiteralIn(window, "/api/admin"); ok {
		t.Fatal("unrelated path must not match")
	}
	if _, ok := pathLiteralIn(`@GetMapping("ers/:id")`, "/users/:id"); ok {
		t.Fatal("suffix must fall on a segment boundary")
	}
}

// TestScanMissedRoutes recall 守卫：LLM 漏报的注册行进疑似遗漏清单，已采纳的注册行不重复报。
func TestScanMissedRoutes(t *testing.T) {
	root := t.TempDir()
	src := "router.get('/reported', a)\nrouter.get('/missed', b)\n" +
		"function a() {}\nfunction b() {}\n" +
		"const notARoute = '/api/plain-config-string';\n"
	file := filepath.Join(root, "routes.js")
	if err := os.WriteFile(file, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	prog, err := NewProgram(root)
	if err != nil {
		t.Fatal(err)
	}
	// 已采纳 /reported@line1；/missed@line2 是漏报；plain-config 不是路由注册行（无动词）。
	accepted := []route{{file: file, line: 1, path: "/reported"}}
	missed := scanMissedRoutes(prog, []string{file}, accepted)
	var got []int
	for _, m := range missed {
		got = append(got, m.Line)
	}
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("missed lines = %v, want [2]; issues=%+v", got, missed)
	}
	if missed[0].RawPath != "/missed" {
		t.Fatalf("missed rawPath = %q, want /missed", missed[0].RawPath)
	}
}

// TestBuildFactsStreamingContentType SSE 流式响应带 contentType 时，挂到对应 media type、
// body 按标量 string 渲染，而非 JSON object。
func TestBuildFactsStreamingContentType(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "events.py")
	src := "async def run_events(run_id: str):\n    return StreamingResponse(stream(), media_type=\"text/event-stream\")\n"
	if err := os.WriteFile(file, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	prog, err := NewProgram(root)
	if err != nil {
		t.Fatal(err)
	}
	o := opOutcome{
		text:    src,
		sources: []infer.SourceSnippet{{File: file, Line: 1, Code: src}},
		contract: &infer.ExtractedContract{
			Responses: []infer.ExtractedResponse{
				{Status: 200, Error: false, ContentType: "text/event-stream", Body: &infer.ShapeNode{Type: "string"}},
			},
		},
	}
	factsOut, rejected := buildFacts(prog, route{method: "GET", path: "/events", handler: "run_events", file: file, line: 1}, o)
	if rejected != 0 {
		t.Fatalf("rejected = %d, want 0", rejected)
	}
	for _, f := range factsOut {
		if cp, ok := f.Contract(); ok && len(cp.Responses) == 1 {
			if cp.Responses[0].ContentType != "text/event-stream" {
				t.Fatalf("contentType = %q, want text/event-stream", cp.Responses[0].ContentType)
			}
			return
		}
	}
	t.Fatal("no contract fact produced")
}

// TestSerializeAliases json tag 名映射：LLM 报 Go 字段名（PascalCase）时，形状核对据 tag 名还原。
func TestSerializeAliases(t *testing.T) {
	text := "type R struct {\n\tData string `json:\"data\"`\n\tId   int    `json:\"id,omitempty\"`\n\tName string `json:\"name\"`\n}\n"
	alias := serializeAliases(text)
	if alias["Data"] != "data" || alias["Id"] != "id" || alias["Name"] != "name" {
		t.Fatalf("serializeAliases = %v", alias)
	}
}

// TestReachDepthCoversLayeredHandler 分层 controller→service→access 是 3 跳：可达深度须覆盖到写出口
// （第 3~4 跳的函数），否则契约切片里没有响应写出口，LLM 只能答 no response found。
func TestReachDepthCoversLayeredHandler(t *testing.T) {
	root := t.TempDir()
	src := map[string]string{
		"ctl.go":   "package routes\nfunc ListCtl() { ListSvc() }\n",
		"svc.go":   "package routes\nfunc ListSvc() { ListAcc() }\n",
		"acc.go":   "package routes\nfunc ListAcc() { Write() }\n",
		"write.go": "package routes\nfunc Write() {}\n",
	}
	for name, body := range src {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prog, err := NewProgram(root)
	if err != nil {
		t.Fatal(err)
	}
	handler := "ctl.go#ListCtl@2"
	reach := prog.Reach(handler)
	// Write() 在第 3 跳；深度 4 必须把它纳入切片。
	if _, ok := reach["write.go#Write@2"]; !ok {
		t.Fatalf("Write() (3 hops away) not reached; reach = %v", keys(reach))
	}
}

func keys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestSplitRouteBlocks 密集路由文件切成小批：>routeBlockTarget 条注册行时切成多块，
// 每块都带文件头（import/挂载前缀），保证单块规模可控、挂载前缀可判定。
func TestSplitRouteBlocks(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "router.go")
	header := "package routes\nimport \"x\"\nfunc Router() {\n\tapp := g.Group(\"/api\")\n"
	var b strings.Builder
	b.WriteString(header)
	n := routeBlockTarget*2 + 10
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "\tapp.Get(\"/r%d\", h%d)\n", i, i)
	}
	b.WriteString("}\n")
	if err := os.WriteFile(file, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	prog, err := NewProgram(root)
	if err != nil {
		t.Fatal(err)
	}
	blocks := splitRouteBlocks(prog, file)
	if len(blocks) < 2 {
		t.Fatalf("splitRouteBlocks = %d blocks, want >= 2", len(blocks))
	}
	for _, blk := range blocks {
		if !strings.Contains(strings.Join(blk, "\n"), "func Router()") {
			t.Fatalf("each block must carry the file header (mount prefix context), got %q", blk)
		}
	}
}

// TestProgramSkipsTestFiles 测试桩文件（_test.go）不进入程序视图：
// 测试里的路由不是真实 API，此前会把 requestOnly_test.go 的 /command、
// authPoint_test.go 的 /history 当真实端点报出。
func TestProgramSkipsTestFiles(t *testing.T) {
	root := t.TempDir()
	for _, f := range []struct {
		name, body string
	}{
		{"routes.go", "package routes\n\nfunc real() {}\n"},
		{"routes_test.go", "package routes\n\nfunc fake() {}\n"},
	} {
		if err := os.WriteFile(filepath.Join(root, f.name), []byte(f.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prog, err := NewProgram(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range prog.Files() {
		if isTestFile(f) {
			t.Fatalf("test file %q leaked into program files", f)
		}
	}
	if !prog.IsSourceFile(filepath.Join(root, "routes.go")) {
		t.Fatal("routes.go should be a source file")
	}
	if prog.IsSourceFile(filepath.Join(root, "routes_test.go")) {
		t.Fatal("routes_test.go must not be a source file")
	}
	if _, ok := prog.Resolve("fake", filepath.Join(root, "routes_test.go")); ok {
		t.Fatal("test-file def must not be resolvable")
	}
}
