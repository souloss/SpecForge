package infer

import (
	"context"
	"encoding/json"
	"os"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/compat_oai/zai"
)

// genkitProvider 用 Genkit 框架 + Z.ai GLM 插件实现 Provider。
//
// 相对旧 HTTPProvider 的差异：
//   - 走 genkit.Init / genkit.Generate，天然带 tracing 与 Dev UI 可观测；
//   - Z.ai 不支持 json_schema 约束解码（zai 插件注释自陈），WithOutputSchema
//     只会把 schema 作为 prompt 指令下发，故「事后结构校验」由上层
//     （EnrichOperation / ResolveGaps）承担，本层只保证返回原始文本字节。
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

// NewGenkitProvider 构造 Genkit Provider。
// 优先读 Z.ai 插件标准环境变量（ZAI_API_KEY / ZAI_BASE_URL），
// 回退到历史 SPECFORGE_LLM_* 变量。APIKey 为空返回 nil（离线模式）。
func NewGenkitProvider() Provider {
	key := firstNonEmpty(os.Getenv("ZAI_API_KEY"), os.Getenv("SPECFORGE_LLM_API_KEY"))
	if key == "" {
		return nil
	}
	// 兼容旧 SPECFORGE_LLM_BASE_URL：zai 插件的 Init 读 ZAI_BASE_URL，
	// 仅在显式配置了旧变量而 ZAI_BASE_URL 未设时桥接一次。
	if os.Getenv("ZAI_BASE_URL") == "" {
		if base := os.Getenv("SPECFORGE_LLM_BASE_URL"); base != "" {
			os.Setenv("ZAI_BASE_URL", base)
		}
	}
	model := envOr("SPECFORGE_LLM_MODEL", "glm-4.6")
	g := genkit.Init(context.Background(),
		genkit.WithPlugins(&zai.ZAI{APIKey: key}),
	)
	return &genkitProvider{
		g:     g,
		model: zai.ModelRef(model, nil),
		name:  "genkit:zai:" + model,
	}
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// envOr 读取环境变量，未设置时返回默认值。
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
