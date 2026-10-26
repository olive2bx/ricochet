package proxy

import (
	"fmt"
	"net"
	"net/netip"
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
		if p == "*" || rules.MatchPattern(p, host) {
			return true
		}
		// CIDR ("10.0.0.0/8") against an IP literal; no DNS lookup.
		if prefix, err := netip.ParsePrefix(p); err == nil {
			if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil && prefix.Contains(addr.Unmap()) {
				return true
			}
		}
	}
	return false
}

// NoProxyPatterns converts NO_PROXY-style entries into bypass patterns:
// "a.com" and ".a.com" match a.com and its subdomains, "*.a.com" only the
// subdomains, "*" every host; IPs and CIDRs are kept. Ports ("a.com:8443")
// are ignored: the whole host skips the upstream.
func NoProxyPatterns(entries []string) []string {
	var out []string
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if _, err := netip.ParsePrefix(e); err == nil {
			out = append(out, e)
			continue
		}
		if h, _, err := net.SplitHostPort(e); err == nil {
			e = h
		}
		e = strings.Trim(e, "[]")
		switch {
		case e == "*", strings.HasPrefix(e, "*."):
			out = append(out, e)
		default:
			e = strings.TrimPrefix(e, ".")
			if _, err := netip.ParseAddr(e); err == nil {
				out = append(out, e)
			} else if e != "" {
				out = append(out, e, "*."+e)
			}
		}
	}
	return out
}

// hostPortDefault appends defPort to h when it has no port.
func hostPortDefault(h, defPort string) string {
	if _, _, err := net.SplitHostPort(h); err == nil {
		return h
	}
	return net.JoinHostPort(strings.Trim(h, "[]"), defPort)
}
