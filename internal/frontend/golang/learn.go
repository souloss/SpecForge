package golang

import (
	"context"
	"log/slog"
	"sort"

	"github.com/specforge/specforge/internal/adapter"
	"github.com/specforge/specforge/internal/infer"
	"github.com/specforge/specforge/internal/profile"
)

// learnSampleOpsMax 画像学习的抽样接口上限（避免一次喂入过多证据拖慢 LLM）。
const learnSampleOpsMax = 5

// learnSnippetsPerOp 画像学习每个抽样接口注入的函数片段数（handler + 直接被调）。
const learnSnippetsPerOp = 3

// learnProfileInto 用 LLM 从抽样接口源码归纳仓库约定，复核后与现有画像合并（仅填补缺失）。
// 学习失败静默降级为原画像（不阻断生成）。
func learnProfileInto(ctx context.Context, prog *Program, prof *profile.Profile, p infer.Provider,
	resolved []adapter.Route, log *slog.Logger) *profile.Profile {
	var samples []infer.SampleOp
	for _, r := range resolved {
		if len(samples) >= learnSampleOpsMax {
			break
		}
		if snips := sampleSnippets(prog, r.Handler); len(snips) > 0 {
			samples = append(samples, infer.SampleOp{Method: r.Method, Path: r.Path, Sources: snips})
		}
	}
	if len(samples) == 0 {
		return prof
	}
	cand, err := infer.LearnProfile(ctx, p, samples)
	if err != nil || cand == nil {
		log.Warn("画像学习失败，沿用原画像", "err", err)
		return prof
	}
	return mergeProfileCandidate(prof, verifyProfileCandidate(prog, cand))
}

// sampleSnippets handler 及其最近的可达函数源码（按调用深度、符号 ID 排序，最多 learnSnippetsPerOp 段）。
func sampleSnippets(prog *Program, handler string) []infer.SourceSnippet {
	reach := prog.Reach(handler)
	ids := make([]string, 0, len(reach))
	for id := range reach {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if reach[ids[i]] != reach[ids[j]] {
			return reach[ids[i]] < reach[ids[j]]
		}
		return ids[i] < ids[j]
	})
	var out []infer.SourceSnippet
	for _, id := range ids {
		if len(out) >= learnSnippetsPerOp {
			break
		}
		if file, start, _, text, ok := prog.FuncSource(id); ok {
			out = append(out, infer.SourceSnippet{Symbol: id, File: prog.RelPath(file), Line: start, Code: text})
		}
	}
	return out
}

// verifyProfileCandidate 在代码图上复核画像候选：汇聚点符号必须是仓库内存在的函数，
// 否则丢弃（LLM 常把包名简写或臆造符号）。
func verifyProfileCandidate(prog *Program, cand *infer.ProfileCandidate) *infer.ProfileCandidate {
	out := *cand
	out.ResponseSinks = nil
	for _, s := range cand.ResponseSinks {
		if prog.SymbolExists(s.Symbol) {
			out.ResponseSinks = append(out.ResponseSinks, s)
		}
	}
	return &out
}

// mergeProfileCandidate 把画像候选合并进现有画像（仅填补缺失，绝不覆盖已有约定）。
func mergeProfileCandidate(prof *profile.Profile, cand *infer.ProfileCandidate) *profile.Profile {
	merged := *prof // 浅拷贝，就地补齐
	if merged.Framework == "" || merged.Framework == "unknown" {
		merged.Framework = cand.Framework
	}
	if len(merged.ResponseSinks) == 0 {
		for _, sc := range cand.ResponseSinks {
			merged.ResponseSinks = append(merged.ResponseSinks, profile.SinkPattern{
				Symbol: sc.Symbol, Signature: sc.Signature,
				DataSlot: sc.DataSlot, ErrSlot: sc.ErrSlot, Status: sc.Status,
			})
		}
	}
	if merged.ResponseEnvelope == nil && cand.Envelope != nil {
		merged.ResponseEnvelope = &profile.EnvelopeSpec{
			Type: cand.Envelope.Type, Properties: cand.Envelope.Properties,
			DataSlot: cand.Envelope.DataSlot, SuccessCode: cand.Envelope.SuccessCode,
		}
	}
	if len(merged.AuthMiddleware) == 0 {
		merged.AuthMiddleware = map[string]profile.SecurityMapping{}
		for name, ac := range cand.AuthMiddleware {
			merged.AuthMiddleware[name] = profile.SecurityMapping{Header: ac.Header, Scheme: ac.Scheme, Required: ac.Required}
		}
	}
	return &merged
}
