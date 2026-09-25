package infer

import (
	"context"
	"encoding/json"
	"os"
	"sync"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/anthropic"
)

// ToolSpec 档位3 子 Agent 可调用的工具规格。
//
// 与 genkit 的 ToolAction 解耦：Call 是闭包，由上层（engine）注入对代码图
// 的只读访问。genkitProvider 负责把它包装成 genkit 工具并注册。
type ToolSpec struct {
	Name        string
	Description string
	// InputSchema 工具入参 JSON Schema；nil 时入参为空对象。
	InputSchema map[string]any
	// Call 工具实现：输入是已按 InputSchema 解码的 map，返回文本结果。
	Call func(input map[string]any) (string, error)
}

// ToolUser 支持工具调用的 Provider（档位3 子 Agent 专用）。
// engine 侧以类型断言判定：非 ToolUser 的 Provider 只能走档位2 模板化。
type ToolUser interface {
	Provider
	// CompleteWithTools 带工具集的多轮生成（工具调用循环由 genkit 的
	// WithMaxTurns 自动驱动）。返回最终文本（期望是 JSON）。
	CompleteWithTools(system, prompt string, schema []byte, tools []ToolSpec, maxTurns int) ([]byte, error)
}

// genkitProvider 用 Genkit 框架 + anthropic 协议网关实现 Provider。
//
// 目标网关满足 anthropic 协议（非官方，是三方网关），认证与端点全部由
// anthropic-sdk-go 的 DefaultClientOptions 从环境变量读取：
//
//	ANTHROPIC_AUTH_TOKEN → authorization: Bearer <token>
//	ANTHROPIC_BASE_URL   → 请求端点
//
// 模型名由 ANTHROPIC_MODEL 单独指定（SDK/插件均不读该变量），默认
// deepseek-v4-pro-0813，作为 model ID 原样透传给网关。
//
// 结构校验分工：网关模型不在 anthropic 官方的 structured-output 白名单内，
// WithOutputSchema 只把 schema 作为 prompt 指令下发（尽力而为），真正的
// 结构校验由上层（EnrichOperation / ResolveGaps）承担，本层只返回原始文本。
type genkitProvider struct {
	g     *genkit.Genkit
	model ai.ModelRef
	name  string

	// toolsMu 保护 tools 注册（同一个 Genkit registry 重复注册同名工具会 panic）。
	toolsMu sync.Mutex
	tools   map[string]ai.ToolRef
}

// Name 实现 Provider。
func (p *genkitProvider) Name() string { return p.name }

// Complete 实现 Provider：system + prompt + 可选输出 schema。
// 返回模型输出的原始文本字节（期望是 JSON，但不做结构校验——校验在上层）。
func (p *genkitProvider) Complete(system, prompt string, schema []byte) ([]byte, error) {
	ctx := context.Background()
	opts := []ai.GenerateOption{
		ai.WithModel(p.model),
		ai.WithSystem(system),
		ai.WithPrompt(prompt),
	}
	if len(schema) > 0 {
		var m map[string]any
		if err := json.Unmarshal(schema, &m); err == nil {
			opts = append(opts, ai.WithOutputSchema(m))
		}
	}
	resp, err := genkit.Generate(ctx, p.g, opts...)
	if err != nil {
		return nil, err
	}
	return []byte(resp.Text()), nil
}

// CompleteWithTools 实现 ToolUser：注册工具后走 genkit 多轮生成（工具循环由
// WithMaxTurns 自动驱动）。工具集按 name 幂等注册，同一 provider 复用。
func (p *genkitProvider) CompleteWithTools(system, prompt string, schema []byte, tools []ToolSpec, maxTurns int) ([]byte, error) {
	refs, err := p.registerTools(tools)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	opts := []ai.GenerateOption{
		ai.WithModel(p.model),
		ai.WithSystem(system),
		ai.WithPrompt(prompt),
		ai.WithMaxTurns(maxTurns),
	}
	if len(refs) > 0 {
		opts = append(opts, ai.WithTools(refs...))
	}
	if len(schema) > 0 {
		var m map[string]any
		if err := json.Unmarshal(schema, &m); err == nil {
			opts = append(opts, ai.WithOutputSchema(m))
		}
	}
	resp, err := genkit.Generate(ctx, p.g, opts...)
	if err != nil {
		return nil, err
	}
	return []byte(resp.Text()), nil
}

// registerTools 把 ToolSpec 转成 genkit 工具并幂等注册，返回 ToolRef 列表。
func (p *genkitProvider) registerTools(tools []ToolSpec) ([]ai.ToolRef, error) {
	p.toolsMu.Lock()
	defer p.toolsMu.Unlock()
	if p.tools == nil {
		p.tools = map[string]ai.ToolRef{}
	}
	var refs []ai.ToolRef
	for _, spec := range tools {
		if existing, ok := p.tools[spec.Name]; ok {
			refs = append(refs, existing)
			continue
		}
		call := spec.Call
		tool := genkit.DefineTool(p.g, spec.Name, spec.Description,
			func(ctx *ai.ToolContext, input map[string]any) (string, error) {
				return call(input)
			},
		)
		p.tools[spec.Name] = tool
		refs = append(refs, tool)
	}
	return refs, nil
}

// NewGenkitProvider 构造 Genkit Provider（anthropic 协议网关）。
// 认证依赖环境变量 ANTHROPIC_AUTH_TOKEN 或 ANTHROPIC_API_KEY，
// 二者皆空返回 nil（离线模式）。
func NewGenkitProvider() Provider {
	token := os.Getenv("ANTHROPIC_AUTH_TOKEN")
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if token == "" && apiKey == "" {
		return nil
	}
	model := envOr("ANTHROPIC_MODEL", "deepseek-v4-pro-0813")
	// 空结构体：认证与端点由 anthropic 插件 Init 与 SDK 的 DefaultClientOptions
	// 从环境变量读取，避免在代码里显式持有 token。
	g := genkit.Init(context.Background(),
		genkit.WithPlugins(&anthropic.Anthropic{}),
	)
	return &genkitProvider{
		g:     g,
		model: anthropic.ModelRef(model, nil),
		name:  "genkit:anthropic:" + model,
		tools: map[string]ai.ToolRef{},
	}
}

// envOr 读取环境变量，未设置时返回默认值。
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
