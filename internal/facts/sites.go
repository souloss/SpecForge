package facts

import (
	"regexp"
	"strings"
)

// siteSep ErrSource 中来源点之间的分隔符。
const siteSep = ", "

// siteEnd 来源点的结尾「@file:line」后紧跟分隔符或串尾：表达式摘要本身可能含逗号，
// 只能以位置后缀为边界切分。
var siteEnd = regexp.MustCompile(`@[^@\s,]+:\d+(?:, |$)`)

// SplitSites 把 ErrSource（「表达式@file:line」以 ", " 连接）拆成来源点；无位置后缀的尾部残段整体作为一个来源点。
func SplitSites(src string) []string {
	var out []string
	start := 0
	for _, m := range siteEnd.FindAllStringIndex(src, -1) {
		end := m[1]
		if strings.HasSuffix(src[m[0]:end], siteSep) {
			end -= len(siteSep)
		}
		if s := strings.TrimSpace(src[start:end]); s != "" {
			out = append(out, s)
		}
		start = m[1]
	}
	if s := strings.TrimSpace(src[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

// JoinSites SplitSites 的逆操作。
func JoinSites(sites []string) string { return strings.Join(sites, siteSep) }

// SiteExpr 来源点去掉「@file:line」位置后缀后的表达式摘要。
func SiteExpr(site string) string {
	if i := strings.LastIndex(site, "@"); i > 0 {
		site = site[:i]
	}
	return strings.TrimSpace(site)
}
