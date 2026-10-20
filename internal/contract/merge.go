package contract

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

// Resolve chooses one candidate only when the values are equivalent. A
// differing set remains visible as a conflict so callers cannot silently lose
// source evidence by changing input order.
//
// A same-status response group mixes success envelopes, error envelopes, and
// status/body variants (e.g. one route writing `code.Response(c, err, data)`
// yields both the `BaseResp` success body and the `ErrResponse` failure body).
// These are not disagreements about the same candidate: they are distinct
// response alternatives. Resolve treats them as such (see mergeResponse),
// so a route with a success+error pair does not degrade to StatusConflict.
func Resolve[T any](candidates []Candidate[T]) (Candidate[T], []Candidate[T]) {
	if len(candidates) == 0 {
		return Candidate[T]{Status: StatusUnresolved}, nil
	}
	if variants, ok := partitionResponseVariants(candidates); ok {
		return variants.resolve()
	}
	return resolveEquivalent(candidates)
}

// partitionResponseVariants 识别「响应变体组」并返回其专属合并逻辑。
//
// 同一状态码下，一条路由的响应行（成功体 code=0、各业务错误码、动态/不带码/未解析错误行、
// 原生写出的成功与错误分支）是**设计上的 oneOf 变体**，不是对同一候选的分歧——按
// 「业务码 + 是否错误分支 + 是否原生写出」归组：组间是变体，组内才做等价合并（组内分歧才是真 conflict）。
func partitionResponseVariants[T any](candidates []Candidate[T]) (*responseVariantGroup[T], bool) {
	if len(candidates) < 2 {
		return nil, false
	}
	classes := map[string]bool{}
	for i := range candidates {
		cv, ok := any(candidates[i].Value).(Response)
		if !ok {
			return nil, false
		}
		classes[responseClassKey(cv)] = true
	}
	// 只有跨「变体类」（业务码/错误分支/原生写出不同）的候选组才是变体；
	// 同类的多个候选仍走等价合并，由 resolveEquivalent 在分歧时判 conflict。
	if len(classes) < 2 {
		return nil, false
	}
	return &responseVariantGroup[T]{candidates: candidates}, true
}

// responseClassKey 响应行的「变体类」键：沿着这几个维度不同的行是互斥的响应形态（oneOf 分支），
// 不参与等价比较。body 形状（Schema/Envelope/DataField 等）不在其中——同类的 body 分歧仍是 conflict。
func responseClassKey(v Response) string {
	return fmt.Sprintf("%s|%d|%t|%t|%t", v.Status, v.Code, v.CodeKnown, v.Failure, v.Raw)
}

// responseVariantGroup 同一状态码下互不相同的响应变体（成功体/错误体/不同 body）。
// 合并目标不是「选一个」而是「都保留」：返回一个把变体归一后的代表候选，
// 全部原始候选以 StatusVerified 保留在 retained 中（调用方据此展开 oneOf，不判 conflict）。
type responseVariantGroup[T any] struct {
	candidates []Candidate[T]
}

// resolve 合并响应变体组：同一变体类的候选做等价合并（分歧则整组返回 conflict），
// 跨类的候选全部保留为变体，状态统一为 StatusVerified。
func (g *responseVariantGroup[T]) resolve() (Candidate[T], []Candidate[T]) {
	// 按变体类归组，组内等价合并（分歧直接判 conflict，不吞掉真实冲突）。
	groups := map[string][]Candidate[T]{}
	var order []string
	for i := range g.candidates {
		cv, _ := any(g.candidates[i].Value).(Response)
		key := responseClassKey(cv)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], g.candidates[i])
	}
	retained := make([]Candidate[T], 0, len(g.candidates))
	for _, key := range order {
		merged, kept := resolveEquivalent(groups[key])
		if merged.Status == StatusConflict {
			return Candidate[T]{Status: StatusConflict}, g.candidates
		}
		// 组内等价合并后取代表候选（组间是变体，各保留一个代表）。
		for i := range kept {
			kept[i].Status = StatusVerified
		}
		if len(kept) > 0 {
			retained = append(retained, kept[0])
		}
	}
	sort.SliceStable(retained, func(i, j int) bool {
		li, lj := candidatePriority(retained[i]), candidatePriority(retained[j])
		if li != lj {
			return li > lj
		}
		return candidateKey(retained[i]) < candidateKey(retained[j])
	})
	resolved := retained[0]
	sort.SliceStable(resolved.Evidence, func(i, j int) bool { return evidenceKey(resolved.Evidence[i]) < evidenceKey(resolved.Evidence[j]) })
	return resolved, retained
}

// resolveEquivalent 原等价合并逻辑（单一候选集合没有变体语义时）。
func resolveEquivalent[T any](candidates []Candidate[T]) (Candidate[T], []Candidate[T]) {
	retained := append([]Candidate[T](nil), candidates...)
	sort.SliceStable(retained, func(i, j int) bool {
		left, right := candidatePriority(retained[i]), candidatePriority(retained[j])
		if left != right {
			return left > right
		}
		return candidateKey(retained[i]) < candidateKey(retained[j])
	})
	resolved := retained[0]
	for _, candidate := range retained[1:] {
		if !equivalent(resolved.Value, candidate.Value) {
			for i := range candidates {
				candidates[i].Status = StatusConflict
			}
			return Candidate[T]{Status: StatusConflict}, candidates
		}
		resolved.Evidence = append(resolved.Evidence, candidate.Evidence...)
		if candidate.Confidence > resolved.Confidence {
			resolved.Confidence = candidate.Confidence
		}
	}
	resolved.Status = strongestStatus(retained)
	sort.SliceStable(resolved.Evidence, func(i, j int) bool { return evidenceKey(resolved.Evidence[i]) < evidenceKey(resolved.Evidence[j]) })
	return resolved, retained
}

func equivalent[T any](left, right T) bool {
	if reflect.DeepEqual(left, right) {
		return true
	}
	// JSON comparison treats semantically identical values consistently even
	// when callers supplied different concrete map types.
	lb, lerr := json.Marshal(left)
	rb, rerr := json.Marshal(right)
	return lerr == nil && rerr == nil && string(lb) == string(rb)
}

func candidatePriority[T any](candidate Candidate[T]) float64 {
	status := map[CandidateStatus]float64{
		StatusVerified:     5,
		StatusDeclared:     4,
		StatusObserved:     3,
		StatusInferred:     2,
		StatusInconclusive: 1,
		StatusUnresolved:   0,
	}
	return sourcePriority(candidate.Evidence, candidate.Status)*10 + status[candidate.Status] + candidate.Confidence
}

// sourcePriority is the explicit cross-source ordering used only to choose the
// representative candidate when values are equivalent. Different values still
// remain a conflict; source priority never hides disagreement.
func sourcePriority(evidence []Evidence, status CandidateStatus) float64 {
	best := 0.0
	for _, item := range evidence {
		value := map[EvidenceSource]float64{
			SourceCode:          5,
			SourceRuntime:       4,
			SourceOpenAPI:       3,
			SourceDocumentation: 2,
			SourceHuman:         2,
			SourceLLM:           1,
		}[item.Source]
		if value > best {
			best = value
		}
	}
	if best == 0 {
		best = map[CandidateStatus]float64{
			StatusVerified: 5,
			StatusObserved: 4,
			StatusDeclared: 3,
			StatusInferred: 1,
		}[status]
	}
	return best
}

func strongestStatus[T any](candidates []Candidate[T]) CandidateStatus {
	if len(candidates) == 0 {
		return StatusUnresolved
	}
	best := candidates[0].Status
	bestScore := candidatePriority(candidates[0]) - candidates[0].Confidence
	for _, candidate := range candidates[1:] {
		score := candidatePriority(candidate) - candidate.Confidence
		if score > bestScore {
			best, bestScore = candidate.Status, score
		}
	}
	return best
}
