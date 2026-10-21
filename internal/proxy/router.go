package proxy

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/olive2bx/ricochet/internal/rules"
)

// Resolver finds the override rule applying to a host. *rules.Store and
// *rules.Set implement it.
type Resolver interface {
	Lookup(host string) (rules.Rule, bool)
}

// RouteKind is the way a connection reaches its destination.
type RouteKind int

const (
	RouteDirect   RouteKind = iota // dial the requested host:port
	RouteOverride                  // dial the IP (and port) forced by a rule
	RouteUpstream                  // go through the upstream proxy
)

// Route is the routing decision for a destination.
type Route struct {
	Kind RouteKind
	// Addr is the address to dial: the override ip:port, the upstream proxy
	// host:port, or the original host:port.
	Addr string
	// Rule is the matching rule for RouteOverride.
	Rule *rules.Rule
}

// String returns "override:<ip>", "upstream:<host:port>" or "direct".
// It never contains credentials.
func (r Route) String() string {
	switch r.Kind {
	case RouteOverride:
		return "override:" + r.Rule.Addr.String()
	case RouteUpstream:
		return "upstream:" + r.Addr
	default:
		return "direct"
	}
}

// Router decides how to reach a destination.
type Router struct {
	rules    Resolver
	upstream string // upstream proxy host:port, "" for none
	bypass   []string
}

// NewRouter builds a router. upstream may be nil.
func NewRouter(res Resolver, upstream *url.URL, bypass []string) *Router {
	rt := &Router{rules: res, bypass: bypass}
	if upstream != nil {
		rt.upstream = hostPortDefault(upstream.Host, "80")
	}
	return rt
}

// Route returns the routing decision for hostport ("host:port").
func (rt *Router) Route(hostport string) (Route, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return Route{}, fmt.Errorf("route %q: %w", hostport, err)
	}
	if rt.rules != nil {
		if rule, ok := rt.rules.Lookup(host); ok {
			if rule.Port != 0 {
				port = strconv.Itoa(int(rule.Port))
			}
			return Route{
				Kind: RouteOverride,
				Addr: net.JoinHostPort(rule.Addr.String(), port),
				Rule: &rule,
			}, nil
		}
	}
	if rt.upstream == "" || rt.bypassed(host) {
		return Route{Kind: RouteDirect, Addr: hostport}, nil
	}
	return Route{Kind: RouteUpstream, Addr: rt.upstream}, nil
}

func (rt *Router) bypassed(host string) bool {
	for _, p := range rt.bypass {
		if rules.MatchPattern(p, host) {
			return true
		}
	}
	return false
}

// hostPortDefault appends defPort to h when it has no port.
func hostPortDefault(h, defPort string) string {
	if _, _, err := net.SplitHostPort(h); err == nil {
		return h
	}
	return net.JoinHostPort(strings.Trim(h, "[]"), defPort)
}
