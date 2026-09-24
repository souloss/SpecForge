package codegraph

import (
	"fmt"
	"go/types"
	"strings"
)

// constID 常量的全限定 ID。
func constID(c *types.Const) string {
	if c.Pkg() != nil {
		return c.Pkg().Path() + "." + c.Name()
	}
	return c.Name()
}

// jsonNameOf 从 struct tag 提取 json 名（无 tag 时返回字段名）。
func jsonNameOf(fieldName, tag string) string {
	v := tagValueOf(tag, "json")
	if v == "" {
		return fieldName
	}
	parts := strings.Split(v, ",")
	name := parts[0]
	if name == "" || name == "-" {
		return name
	}
	return name
}

// hasOmitempty json tag 是否含 omitempty。
func hasOmitempty(tag string) bool {
	v := tagValueOf(tag, "json")
	return containsSplit(v, "omitempty")
}

func containsSplit(v, want string) bool {
	for _, p := range strings.Split(v, ",") {
		if strings.TrimSpace(p) == want {
			return true
		}
	}
	return false
}

// tagValueOf 从 Go struct tag 提取指定 key 的值。
func tagValueOf(tag, key string) string {
	body := strings.Trim(tag, "`")
	for _, seg := range splitTagSegs(body) {
		if strings.HasPrefix(seg, key+":") {
			return strings.Trim(strings.TrimPrefix(seg, key+":"), `"`)
		}
	}
	return ""
}

func splitTagSegs(body string) []string {
	var out []string
	inQuote := false
	var cur strings.Builder
	for _, r := range body {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// Evidence 证据辅助构造（file:line → 哈希校验）。
func (g *Graph) EvidenceAt(file string, line int) string {
	return fmt.Sprintf("%s:%d", file, line)
}
