package proxy

import (
	"net/url"
	"strings"
	"testing"

	"github.com/olive2bx/ricochet/internal/rules"
)

func mustRules(t *testing.T, s string) *rules.Set {
	t.Helper()
	set, err := rules.Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("parse rules: %v", err)
	}
	return set
}

func TestRouter(t *testing.T) {
	set := mustRules(t, `
10.1.2.3        api.example.com
10.1.2.4        *.internal.example.com
10.1.2.5:8443   svc.example.com
2001:db8::1     v6.example.com
`)
	up := &url.URL{Scheme: "http", Host: "upstream.local:3128"}
	bypass := []string{"intranet.local", "*.corp.local"}

	tests := []struct {
		name     string
		upstream *url.URL
		hostport string
		kind     RouteKind
		addr     string
		str      string
	}{
		{"override keeps port", up, "api.example.com:443", RouteOverride, "10.1.2.3:443", "override:10.1.2.3"},
		{"override http port", up, "API.example.com:80", RouteOverride, "10.1.2.3:80", "override:10.1.2.3"},
		{"wildcard override", up, "x.internal.example.com:8080", RouteOverride, "10.1.2.4:8080", "override:10.1.2.4"},
		{"port override", up, "svc.example.com:443", RouteOverride, "10.1.2.5:8443", "override:10.1.2.5"},
		{"ipv6 override", up, "v6.example.com:443", RouteOverride, "[2001:db8::1]:443", "override:2001:db8::1"},
		{"override wins over bypass and no upstream", nil, "api.example.com:443", RouteOverride, "10.1.2.3:443", "override:10.1.2.3"},
		{"no rule goes upstream", up, "other.example.com:443", RouteUpstream, "upstream.local:3128", "upstream:upstream.local:3128"},
		{"no rule no upstream is direct", nil, "other.example.com:443", RouteDirect, "other.example.com:443", "direct"},
		{"bypass exact", up, "intranet.local:80", RouteDirect, "intranet.local:80", "direct"},
		{"bypass wildcard", up, "a.corp.local:443", RouteDirect, "a.corp.local:443", "direct"},
		{"bypass wildcard excludes bare domain", up, "corp.local:443", RouteUpstream, "upstream.local:3128", "upstream:upstream.local:3128"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := NewRouter(set, tt.upstream, bypass)
			r, err := rt.Route(tt.hostport)
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if r.Kind != tt.kind || r.Addr != tt.addr || r.String() != tt.str {
				t.Errorf("Route(%q) = {%v %q %q}, want {%v %q %q}", tt.hostport, r.Kind, r.Addr, r.String(), tt.kind, tt.addr, tt.str)
			}
			if (r.Kind == RouteOverride) != (r.Rule != nil) {
				t.Errorf("Rule = %v for kind %v", r.Rule, r.Kind)
			}
		})
	}
}

func TestRouterDefaultUpstreamPort(t *testing.T) {
	rt := NewRouter(nil, &url.URL{Scheme: "http", Host: "proxy.local"}, nil)
	r, err := rt.Route("a.com:443")
	if err != nil {
		t.Fatal(err)
	}
	if r.Addr != "proxy.local:80" {
		t.Errorf("Addr = %q, want proxy.local:80", r.Addr)
	}
}

func TestRouterInvalidHostPort(t *testing.T) {
	if _, err := NewRouter(nil, nil, nil).Route("no-port"); err == nil {
		t.Error("Route without port succeeded")
	}
}

func TestNoProxy(t *testing.T) {
	up := &url.URL{Scheme: "http", Host: "upstream.local:3128"}
	patterns := NoProxyPatterns([]string{" Corp.example ", ".lab.example", "*.wild.example", "svc.example:8443", "10.0.0.0/8", "192.168.1.5", "[::1]", ""})
	tests := []struct {
		host   string
		direct bool
	}{
		{"corp.example", true},
		{"a.b.corp.example", true},
		{"notcorp.example", false},
		{"lab.example", true},
		{"x.lab.example", true},
		{"wild.example", false},
		{"x.wild.example", true},
		{"svc.example", true},
		{"10.20.30.40", true},
		{"11.0.0.1", false},
		{"192.168.1.5", true},
		{"[::1]", true},
		{"other.example", false},
	}
	rt := NewRouter(nil, up, patterns)
	for _, tt := range tests {
		r, err := rt.Route(tt.host + ":443")
		if err != nil {
			t.Fatalf("Route(%q): %v", tt.host, err)
		}
		if (r.Kind == RouteDirect) != tt.direct {
			t.Errorf("%s: route %s, direct=%v wanted", tt.host, r, tt.direct)
		}
	}

	all := NewRouter(nil, up, NoProxyPatterns([]string{"*"}))
	if r, _ := all.Route("anything.example:443"); r.Kind != RouteDirect {
		t.Errorf(`"*": route %s, want direct`, r)
	}
}
