package facts

import (
	"reflect"
	"testing"
)

// TestSplitSites 表达式摘要含逗号时仍按「@file:line」边界切分，且与 JoinSites 互逆。
func TestSplitSites(t *testing.T) {
	sites := []string{
		`errors.Errorf("query %s %s error response, %s", serviceName,…@svc/a.go:12`,
		`res.Error.Code@svc/b.go:30`,
		`db.First(x).Error@repo/c.go:7`,
	}
	if got := SplitSites(JoinSites(sites)); !reflect.DeepEqual(got, sites) {
		t.Fatalf("SplitSites = %q, want %q", got, sites)
	}
	if got := SplitSites("a@x.go:1, trailing"); !reflect.DeepEqual(got, []string{"a@x.go:1", "trailing"}) {
		t.Fatalf("trailing fragment: %q", got)
	}
	if got := SiteExpr(sites[1]); got != "res.Error.Code" {
		t.Fatalf("SiteExpr = %q", got)
	}
}
