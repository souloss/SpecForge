package facts

import (
	"sort"
	"strings"
)

// 前端与流水线共享的 IR 词汇：前端按此产出缺口与占位码，流水线（LLM 兜底）按此识别可闭合的缺口。

// UnresolvedCode 信封码占位：该错误行的码无法静态确定（排序置于最后，LLM 兜底的目标行）。
const UnresolvedCode = -1

// GapAnyMarker any 类缺口描述中的标记子串。
const GapAnyMarker = "any/interface{}"

// GapErrorPrefix 「错误码未解析」缺口的描述前缀。
const GapErrorPrefix = "error envelope:"

// GapErrorUnresolved 错误变量无法静态反推到常量时的缺口描述。
const GapErrorUnresolved = GapErrorPrefix + " err variable not statically resolved"

// GapSuccessAny 成功响应 data 为 any 且不可定型时的缺口描述（前缀部分用于匹配）。
const GapSuccessAny = "success data: " + GapAnyMarker + " cannot be typed"

// gapSchemaAnyPrefix / gapSchemaAnySuffix 「schema 路径上的值为 any」缺口的描述格式：
// "schema <类型 ID>: <路径> is interface{}/any in source"（路径 <root> 表示 schema 本身）。
const (
	gapSchemaAnyPrefix = "schema "
	gapSchemaAnySep    = ": "
	gapSchemaAnySuffix = " is " + "interface{}/any" + " in source"
	// GapRootPath 缺口路径中表示 schema 根的记号。
	GapRootPath = "<root>"
)

// GapSchemaAny 构造「schema 路径为 any」缺口描述。
func GapSchemaAny(schemaID, path string) string {
	return gapSchemaAnyPrefix + schemaID + gapSchemaAnySep + path + gapSchemaAnySuffix
}

// ParseSchemaAnyGap 解析 GapSchemaAny 产生的描述。
func ParseSchemaAnyGap(gap string) (schemaID, path string, ok bool) {
	if !strings.HasPrefix(gap, gapSchemaAnyPrefix) || !strings.HasSuffix(gap, gapSchemaAnySuffix) {
		return "", "", false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(gap, gapSchemaAnyPrefix), gapSchemaAnySuffix)
	i := strings.LastIndex(body, gapSchemaAnySep)
	if i < 0 {
		return "", "", false
	}
	return body[:i], body[i+len(gapSchemaAnySep):], true
}

// 核对强度（LLM 事实的置信度上限由此决定）：静态事实为空。
const (
	VerifyTyped  = "typed"  // 以类型/符号信息核对（如错误码常量必须在切片中出现且命中目录）
	VerifySymbol = "symbol" // 以签名核对结构、以文本核对名字（如包装器摘要）
	VerifyText   = "text"   // 只能以源码文本核对（字段名出现在源码中、证据行存在）
)

// VerificationCap 各核对强度下 LLM 事实的置信度上限（静态事实为 1.0）。
func VerificationCap(v string) float64 {
	switch v {
	case VerifyTyped:
		return capTyped
	case VerifySymbol:
		return capSymbol
	case VerifyText:
		return capText
	}
	return 1.0
}

// 置信度上限：typed 高于低置信线（0.8），text 低于它（进入报告待人审）。
const (
	capTyped  = 0.8
	capSymbol = 0.7
	capText   = 0.6
)

// GapWrapperPrefix 「写出函数没被识别为响应包装器」缺口的描述前缀（后接函数符号 ID）。
const GapWrapperPrefix = "response wrapper not summarized: "

// SortResponses 响应行稳定排序：成功（码 0）在前，业务错误码升序，未解析行最后，同码按写出点。
func SortResponses(rows []ResponseFact) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if (a.Envelope == nil) != (b.Envelope == nil) {
			return a.Envelope != nil
		}
		if a.Envelope != nil && b.Envelope != nil {
			if a.Envelope.Code == b.Envelope.Code && a.Failure != b.Failure {
				return !a.Failure // 同码：成功体在前、错误分支在后
			}
			if a.Envelope.Code != b.Envelope.Code {
				// 成功(0)最前; -1(未解析)最后; 其余升序
				if a.Envelope.Code == 0 {
					return true
				}
				if b.Envelope.Code == 0 {
					return false
				}
				if a.Envelope.Code == UnresolvedCode {
					return false
				}
				if b.Envelope.Code == UnresolvedCode {
					return true
				}
				return a.Envelope.Code < b.Envelope.Code
			}
		}
		return a.Sink < b.Sink
	})
}
