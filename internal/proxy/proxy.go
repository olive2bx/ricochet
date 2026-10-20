// Package proxy wires goproxy with the FQDN override router and the
// upstream proxy chaining.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/elazarl/goproxy"
)

// Config configures a Proxy.
type Config struct {
	// Rules provides FQDN → IP overrides. May be nil.
	Rules Resolver
	// Upstream is the upstream HTTP proxy. Nil means direct connections.
	// Any userinfo is ignored: use UpstreamAuth.
	Upstream *url.URL
	// UpstreamAuth is "user:pass" sent as Proxy-Authorization to Upstream.
	UpstreamAuth string
	// Bypass lists hosts ("a.com", "*.b.com") that skip the upstream.
	Bypass []string
	// Logger receives routing logs. Defaults to slog.Default().
	Logger *slog.Logger
}

// Proxy is an HTTP proxy handler.
type Proxy struct {
	router *Router
	auth   string
	log    *slog.Logger
	dialer net.Dialer
	tr     *http.Transport
	gp     *goproxy.ProxyHttpServer
}

// New builds a Proxy.
func New(cfg Config) (*Proxy, error) {
	var upstream *url.URL
	if cfg.Upstream != nil {
		if cfg.Upstream.Scheme != "http" {
			return nil, fmt.Errorf("upstream %q: only http:// proxies are supported", cfg.Upstream.Redacted())
		}
		if cfg.Upstream.Host == "" {
			return nil, errors.New("upstream: missing host")
		}
		// Strip credentials so they can never leak through logs or routes.
		upstream = &url.URL{Scheme: "http", Host: cfg.Upstream.Host}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	p := &Proxy{
		router: NewRouter(cfg.Rules, upstream, cfg.Bypass),
		auth:   cfg.UpstreamAuth,
		log:    logger,
		dialer: net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second},
	}

	p.tr = &http.Transport{
		Proxy:                 p.transportProxy,
		DialContext:           p.dialOverride,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	gp := goproxy.NewProxyHttpServer()
	gp.Logger = slogPrintf{logger}
	gp.Tr = p.tr
	gp.ConnectDial = nil
	gp.ConnectDialWithReq = p.connectDial
	gp.NonproxyHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "ricochet is an HTTP proxy: use it with curl -x / HTTPS_PROXY", http.StatusBadRequest)
	})
	p.gp = gp
	return p, nil
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) { p.gp.ServeHTTP(w, r) }

// Router returns the proxy router.
func (p *Proxy) Router() *Router { return p.router }

// CloseIdleConnections drops pooled connections, e.g. after a rules change.
func (p *Proxy) CloseIdleConnections() { p.tr.CloseIdleConnections() }

// transportProxy implements http.Transport.Proxy for plain HTTP requests.
func (p *Proxy) transportProxy(req *http.Request) (*url.URL, error) {
	route, err := p.router.Route(hostPortDefault(req.URL.Host, defaultPort(req.URL.Scheme)))
	if err != nil {
		return nil, err
	}
	p.log.Debug("route", "scheme", req.URL.Scheme, "host", req.URL.Host, "route", route.String(), "rule", ruleText(route))
	if route.Kind != RouteUpstream {
		return nil, nil
	}
	u := &url.URL{Scheme: "http", Host: route.Addr}
	if p.auth != "" {
		// http.Transport turns userinfo into Proxy-Authorization.
		user, pass, _ := strings.Cut(p.auth, ":")
		u.User = url.UserPassword(user, pass)
	}
	return u, nil
}

// dialOverride is the Transport dialer: it applies override rules and dials
// everything else (target or upstream proxy) as is. The URL, Host header and
// SNI are untouched since only the dialed address changes.
func (p *Proxy) dialOverride(ctx context.Context, network, addr string) (net.Conn, error) {
	route, err := p.router.Route(addr)
	if err == nil && route.Kind == RouteOverride {
		addr = route.Addr
	}
	conn, err := p.dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return conn, nil
}

// connectDial opens the tunnel for a CONNECT request.
func (p *Proxy) connectDial(req *http.Request, network, addr string) (net.Conn, error) {
	ctx := req.Context()
	route, err := p.router.Route(addr)
	if err != nil {
		return nil, err
	}
	p.log.Debug("route", "scheme", "connect", "host", addr, "route", route.String(), "rule", ruleText(route))

	conn, err := p.dialer.DialContext(ctx, network, route.Addr)
	if err != nil {
		p.log.Warn("dial failed", "host", addr, "route", route.String(), "err", err)
		return nil, fmt.Errorf("dial %s (%s): %w", addr, route, err)
	}
	if route.Kind != RouteUpstream {
		return conn, nil
	}
	tunnel, err := connectThrough(ctx, conn, addr, p.auth)
	if err != nil {
		p.log.Warn("upstream CONNECT failed", "host", addr, "route", route.String(), "err", err)
		return nil, err
	}
	return tunnel, nil
}

func ruleText(r Route) string {
	if r.Rule == nil {
		return ""
	}
	return r.Rule.String()
}

func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

// slogPrintf adapts slog to goproxy's Printf logger (only used when
// goproxy.Verbose is set, or for its warnings, which duplicate ours).
type slogPrintf struct{ l *slog.Logger }

func (s slogPrintf) Printf(format string, v ...any) {
	s.l.Debug(strings.TrimSpace(fmt.Sprintf(format, v...)), "component", "goproxy")
}
