package infer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/anthropic"
)

// defaultModel ANTHROPIC_MODEL 未设置时透传给网关的模型 ID。
const defaultModel = "deepseek-v4-pro-0813"

// defaultCallTimeout 单次调用（含工具循环）的默认超时：保留深度思考时单 op 实测 50s+，
// 60s 会误杀有效产出；SPECFORGE_LLM_TIMEOUT 可覆盖（如 "60s"）。
const defaultCallTimeout = 180 * time.Second

// envLLMTimeout / envLLMThinking / envModel 环境变量名。
const (
	envLLMTimeout  = "SPECFORGE_LLM_TIMEOUT"
	envLLMThinking = "SPECFORGE_LLM_THINKING"
	envModel       = "ANTHROPIC_MODEL"
	envAuthToken   = "ANTHROPIC_AUTH_TOKEN"
	envAPIKey      = "ANTHROPIC_API_KEY"
)

// thinkingOff SPECFORGE_LLM_THINKING 取此值时显式关闭深度思考。
const thinkingOff = "off"

// genkitProvider 用 Genkit 框架 + anthropic 协议网关实现 Provider。
//
// 目标网关满足 anthropic 协议（三方网关），认证与端点由 anthropic-sdk-go 的
// DefaultClientOptions 从环境变量读取（ANTHROPIC_AUTH_TOKEN / ANTHROPIC_BASE_URL），
// 模型名由 ANTHROPIC_MODEL 指定，作为 model ID 原样透传。
//
// 结构校验分工：网关模型不在官方 structured-output 白名单内，schema 由任务协议层
// 内嵌到 prompt（尽力而为），真正的结构校验在任务协议层事后承担，本层只返回原始文本。
type genkitProvider struct {
	g       *genkit.Genkit // genkit 实例（持有工具 registry）
	model   ai.ModelRef    // 模型引用（含 thinking 配置）
	name    string         // 供应方标识（计入缓存键）
	timeout time.Duration  // 单次调用超时

	toolsMu sync.Mutex            // 保护 tools（同名工具重复注册会 panic）
	tools   map[string]ai.ToolRef // 已注册的分发壳（按名）

	usageMu sync.Mutex // 保护 usage（并发兜底时多 goroutine 累加）
	usage   Usage      // 累计用量
}

// Name 实现 Provider。
func (p *genkitProvider) Name() string { return p.name }

// Complete 实现 Provider：system + prompt（+ 可选输出 schema、工具循环）。
// 超时与取消由 ctx 与 p.timeout 共同约束；工具经 ctx 分发到本次请求的实现。
func (p *genkitProvider) Complete(parent context.Context, req Request) (Response, error) {
	ctx, cancel := context.WithTimeout(parent, p.timeout)
	defer cancel()
	opts := []ai.GenerateOption{
		ai.WithModel(p.model),
		ai.WithSystem(req.System),
		ai.WithPrompt(req.Prompt),
	}
	if len(req.Schema) > 0 {
		var m map[string]any
		if err := json.Unmarshal(req.Schema, &m); err == nil {
			opts = append(opts, ai.WithOutputSchema(m))
		}
	}
	if len(req.Tools) > 0 {
		opts = append(opts, ai.WithTools(p.registerTools(req.Tools)...), ai.WithMaxTurns(req.MaxTurns))
		if ctx.Value(toolCtxKey{}) == nil { // 外层（CachedProvider）已建作用域时复用，以便其取读集
			ctx, _ = withToolScope(ctx, req.Tools)
		}
	}
	resp, err := genkit.Generate(ctx, p.g, opts...)
	// 工具循环耗尽轮数，或本次时限到期而外层未取消：交上层退回无工具单次调用（重新计时）。
	if len(req.Tools) > 0 && (errors.Is(err, ai.ErrMaxTurnsExceeded) ||
		errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil) {
		p.addUsage(usageOf(resp)) // 已消耗的轮次照样计量
		return Response{}, fmt.Errorf("%w: %v", ErrToolBudgetExhausted, err)
	}
	if err != nil {
		return Response{}, err
	}
	u := usageOf(resp)
	p.addUsage(u)
	return Response{Text: []byte(resp.Text()), Usage: u}, nil
}

// addUsage 累加用量（并发安全）。
func (p *genkitProvider) addUsage(u Usage) {
	p.usageMu.Lock()
	p.usage.Add(u)
	p.usageMu.Unlock()
}

// usageOf 一次生成响应的用量（网关未上报时仅计调用次数）。
func usageOf(resp *ai.ModelResponse) Usage {
	u := Usage{Calls: 1}
	if resp != nil && resp.Usage != nil {
		u.InputTokens = int64(resp.Usage.InputTokens)
		u.OutputTokens = int64(resp.Usage.OutputTokens)
	}
	return u
}

// LLMUsage 实现 UsageReporter。
func (p *genkitProvider) LLMUsage() Usage {
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	return p.usage
}

// registerTools 按名幂等注册工具分发壳（实现经 ctx 分发，见 toolScope），返回 ToolRef 列表。
func (p *genkitProvider) registerTools(tools []ToolSpec) []ai.ToolRef {
	p.toolsMu.Lock()
	defer p.toolsMu.Unlock()
	refs := make([]ai.ToolRef, 0, len(tools))
	for _, spec := range tools {
		if existing, ok := p.tools[spec.Name]; ok {
			refs = append(refs, existing)
			continue
		}
		name := spec.Name
		var topts []ai.ToolOption
		if spec.InputSchema != nil {
			topts = append(topts, ai.WithInputSchema(spec.InputSchema))
		}
		tool := genkit.DefineTool(p.g, name, spec.Description,
			// 显式 input schema 时 genkit 要求入参类型为 any：统一转成对象再分发。
			func(tc *ai.ToolContext, input any) (string, error) {
				m, _ := input.(map[string]any)
				return dispatchTool(tc.Context, name, m)
			}, topts...)
		p.tools[name] = tool
		refs = append(refs, tool)
	}
	return refs
}

// NewGenkitProvider 构造 Genkit Provider；ANTHROPIC_AUTH_TOKEN 与 ANTHROPIC_API_KEY 皆空返回 nil（离线）。
//
// thinking 默认沿用网关深度思考：实测关闭会让「从目录选错误码」类结构化任务退化成空答；
// SPECFORGE_LLM_THINKING=off 时显式关闭（更快，适合已有证据注入的简单缺口）。
func NewGenkitProvider() Provider {
	if os.Getenv(envAuthToken) == "" && os.Getenv(envAPIKey) == "" {
		return nil
	}
	model := envOr(envModel, defaultModel)
	// 空结构体：认证与端点由 anthropic 插件与 SDK 的 DefaultClientOptions 从环境变量读取。
	// genkit 初始化会往全局 slog 打 INFO 日志（"Genkit initialized"）：临时静音，保持 stderr 只有本工具的日志。
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	g := genkit.Init(context.Background(), genkit.WithPlugins(&anthropic.Anthropic{}))
	slog.SetDefault(prev)
	cfg := &anthropicsdk.MessageNewParams{}
	if os.Getenv(envLLMThinking) == thinkingOff {
		disabled := anthropicsdk.NewThinkingConfigDisabledParam()
		cfg.Thinking = anthropicsdk.ThinkingConfigParamUnion{OfDisabled: &disabled}
	}
	return &genkitProvider{
		g:       g,
		model:   anthropic.ModelRef(model, cfg),
		name:    "genkit:anthropic:" + model,
		timeout: llmTimeout(),
		tools:   map[string]ai.ToolRef{},
	}
}

// envOr 读取环境变量，未设置时返回默认值。
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// llmTimeout 单次调用超时：SPECFORGE_LLM_TIMEOUT 可覆盖默认值。
func llmTimeout() time.Duration {
	if v := os.Getenv(envLLMTimeout); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return defaultCallTimeout
}
