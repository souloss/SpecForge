// Package infer 的 LLM 提示词资产：全部 system prompt / 输出 schema / 示例以独立文件
// 存放在 prompts/ 子目录，经 go:embed 打进二进制，运行时按名读取。
//
// 提示词是版本管理的一等公民：修改措辞、规则或输出 schema 只改 prompts/ 下对应文件，
// 与代码逻辑解耦，diff 可读、可回滚、可做 PR 审阅。约定：
//   - *.system.md   纯 system prompt（规则 + 任务说明）
//   - *.schema.json 输出 JSON Schema（与 *.system.md 同名配对，由代码拼接在 prompt 末尾）
//   - *.example.json 输出示例（防止模型照抄；与 *.system.md 配对）
//   - *.append.md   追加到用户消息的片段（如工具耗尽提示）
//   - *.template.md fmt.Sprintf 模板（含 %v 等占位符）
package infer

import (
	"embed"
	"sync"
)

// promptFS 内嵌的提示词资产。
//
//go:embed prompts/*.md prompts/*.json
var promptFS embed.FS

// promptCache 已读入的提示词内容（prompt 按名读取一次后缓存，避免每次请求组装重复解压）。
var promptCache sync.Map // name → string

// prompt 按相对 prompts/ 的文件名读取内嵌提示词；缺文件是编译期错误，运行时 panic 暴露。
func prompt(name string) string {
	if v, ok := promptCache.Load(name); ok {
		return v.(string)
	}
	b, err := promptFS.ReadFile("prompts/" + name)
	if err != nil {
		panic("infer: embedded prompt asset " + name + " missing: " + err.Error())
	}
	s := string(b)
	promptCache.Store(name, s)
	return s
}
