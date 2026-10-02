package golang

import (
	"strings"
	"testing"

	"github.com/specforge/specforge/internal/adapter"
	"github.com/specforge/specforge/internal/profile"
)

func TestWildcardExpansionKeepsUnsupportedConnectGap(t *testing.T) {
	routes := []adapter.Route{
		{Method: "GET", Path: "/proxy/{wildcard}", RawPath: "/proxy/*", Handler: "proxy.Handler", Unresolved: adapter.ReasonWildcard},
		{Method: "CONNECT", Path: "/proxy/{wildcard}", RawPath: "/proxy/*", Handler: "proxy.Handler", Unresolved: adapter.ReasonUnsupportedMethod},
	}
	got := expandWildcards(routes, profile.Default())
	methods := make(map[string]adapter.Route, len(got))
	for _, route := range got {
		methods[route.Method] = route
	}
	if route := methods["GET"]; route.Unresolved != "" || route.Path != "/proxy/{wildcard}" {
		t.Fatalf("GET wildcard route = %+v", route)
	}
	if route := methods["CONNECT"]; route.Unresolved != adapter.ReasonUnsupportedMethod {
		t.Fatalf("CONNECT wildcard route = %+v", route)
	}
}

func TestRouteIdentityStatsAndUnresolvedDetails(t *testing.T) {
	routes := []adapter.Route{
		{Method: "GET", Path: "/Ping", RawPath: "/Ping", File: "/repo/router.go", Line: 10},
		{Method: "GET", Path: "/Ping", RawPath: "/Ping", File: "/repo/server.go", Line: 20},
		{Method: "GET", Path: "/", RawPath: "/", File: "/repo/server.go", Line: 21, Unresolved: adapter.ReasonNoHandler},
	}
	unique, collapsed := routeIdentityStats(routes)
	issues := unresolvedRouteIssues(routes, func(path string) string { return strings.TrimPrefix(path, "/repo/") })
	if unique != 2 || collapsed != 1 {
		t.Fatalf("unique/collapsed = %d/%d, want 2/1", unique, collapsed)
	}
	if len(issues) != 1 || issues[0].Path != "/" || issues[0].File != "server.go" || issues[0].Line != 21 || issues[0].Reason != adapter.ReasonNoHandler {
		t.Fatalf("unresolved route details = %+v", issues)
	}
	facts := unresolvedRouteFacts(routes)
	if len(facts) != 1 || facts[0].ID != "contract:op:GET:/" {
		t.Fatalf("unresolved route contract facts = %+v", facts)
	}
	contract, ok := facts[0].Contract()
	if !ok || len(contract.Gaps) != 1 || len(contract.Responses) != 1 || !contract.Responses[0].Raw || contract.Responses[0].HasBody {
		t.Fatalf("unresolved route was not represented conservatively: %+v", facts[0])
	}
}
