package infer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
)

// ToolSpec 档位3 子 Agent 可调用的工具规格（对代码图的只读查询）。
//
// Call 必须是纯函数：同样的代码 + 同样的输入 → 同样的输出。缓存层靠「重放工具调用、
// 比对输出哈希」判定一条 LLM 结果是否仍然有效（Salsa 式读集校验），非纯工具会让缓存永不命中。
type ToolSpec struct {
	Name        string         // 工具名（模型据此发起调用）
	Description string         // 工具用途说明（模型据此决定何时调用）
	InputSchema map[string]any // 入参 JSON Schema；nil 时入参为任意对象
	// Call 工具实现：输入是模型给出的入参对象，返回文本结果（通常为 JSON）。
	Call func(ctx context.Context, input map[string]any) (string, error)
}

// ToolRead 一次工具调用的读集记录：工具名 + 规范化入参 + 输出指纹。
type ToolRead struct {
	Tool    string `json:"tool"`     // 工具名
	Input   string `json:"input"`    // 规范化 JSON 入参（键有序，encoding/json 保证）
	OutHash string `json:"out_hash"` // 输出文本的 sha256
}

// toolCtxKey 上下文键：本次生成的工具表 + 读集记录器。
type toolCtxKey struct{}

// toolScope 一次生成请求的工具作用域：按名分发到本次请求的实现，并记录读集。
//
// genkit 的工具按名注册在进程级 registry 里且不可重复注册；实现若固化在首次注册的
// 闭包里，后续请求（不同代码图）会调用到旧实现。故注册的只是分发壳，真实实现经 ctx 取得。
type toolScope struct {
	impls map[string]ToolSpec // 工具名 → 本次请求的实现
	mu    sync.Mutex          // 保护 reads（模型可能并行发起工具调用）
	reads []ToolRead          // 本次请求实际发生的工具调用（按发生顺序）
}

// withToolScope 把工具表挂到 ctx 上，返回新 ctx 与作用域（用于事后取读集）。
func withToolScope(ctx context.Context, tools []ToolSpec) (context.Context, *toolScope) {
	sc := &toolScope{impls: map[string]ToolSpec{}}
	for _, t := range tools {
		sc.impls[t.Name] = t
	}
	return context.WithValue(ctx, toolCtxKey{}, sc), sc
}

// dispatchTool 注册壳的统一入口：从 ctx 取本次请求的实现执行，并记录读集。
func dispatchTool(ctx context.Context, name string, input map[string]any) (string, error) {
	sc, _ := ctx.Value(toolCtxKey{}).(*toolScope)
	if sc == nil {
		return `{"error":"tool called outside of a generation scope"}`, nil
	}
	impl, ok := sc.impls[name]
	if !ok || impl.Call == nil {
		return `{"error":"unknown tool"}`, nil
	}
	out, err := impl.Call(ctx, input)
	if err != nil {
		return "", err
	}
	sc.mu.Lock()
	sc.reads = append(sc.reads, ToolRead{Tool: name, Input: canonicalJSON(input), OutHash: hashText(out)})
	sc.mu.Unlock()
	return out, nil
}

// Reads 本次请求的读集副本。
func (sc *toolScope) Reads() []ToolRead {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return append([]ToolRead(nil), sc.reads...)
}

// canonicalJSON 规范化 JSON（encoding/json 对 map 键排序），用于读集记录与缓存键。
func canonicalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// hashText 文本的 sha256 十六进制串。
func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// jsonFence Markdown 代码围栏标记（模型常把 JSON 包在 ```json 围栏里）。
var jsonFence = []byte("```")

// extractJSON 从模型输出里取出 JSON 对象：去掉围栏与前后说明文字，取首个 '{' 到末个 '}'。
// 取不到时原样返回，交由调用方的 json.Unmarshal 报错。
func extractJSON(b []byte) []byte {
	b = bytes.TrimSpace(b)
	if bytes.HasPrefix(b, jsonFence) {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
		b = bytes.TrimSuffix(bytes.TrimSpace(b), jsonFence)
	}
	start, end := bytes.IndexByte(b, '{'), bytes.LastIndexByte(b, '}')
	if start < 0 || end < start {
		return b
	}
	return b[start : end+1]
}
