package infer

import (
	"context"
	"encoding/json"
	"os"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/anthropic"
)

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
	}
}

// envOr 读取环境变量，未设置时返回默认值。
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
