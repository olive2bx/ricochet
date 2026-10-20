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

	"github.com/olive2bx/ricochet/internal/collector"
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

	// Collector receives one event per request or tunnel. May be nil.
	Collector *collector.Collector
	// MaskHeaders lists headers whose values are hidden in events. Nil
	// means DefaultMaskedHeaders; an empty non-nil slice masks nothing.
	MaskHeaders []string
	// CaptureBodies is the max number of body bytes stored per request and
	// per response (plain HTTP and MITM only). 0 disables capture.
	CaptureBodies int
	// NoKeepAlive disables connection reuse towards servers so that every
	// request has its own DNS/connect/TLS timings.
	NoKeepAlive bool
}

// Proxy is an HTTP proxy handler.
type Proxy struct {
	router *Router
	auth   string
	log    *slog.Logger
	dialer net.Dialer
	tr     *http.Transport
	gp     *goproxy.ProxyHttpServer

	col          *collector.Collector
	mask         []string
	captureLimit int
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

		col:          cfg.Collector,
		captureLimit: max(0, cfg.CaptureBodies),
	}
	mask := cfg.MaskHeaders
	if mask == nil {
		mask = DefaultMaskedHeaders
	}
	for _, h := range mask {
		if h = strings.TrimSpace(h); h != "" {
			p.mask = append(p.mask, http.CanonicalHeaderKey(h))
		}
	}

	p.tr = &http.Transport{
		Proxy:                 p.transportProxy,
		DialContext:           p.dialOverride,
		ForceAttemptHTTP2:     false,
		DisableKeepAlives:     cfg.NoKeepAlive,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	gp := goproxy.NewProxyHttpServer()
	gp.Logger = slogPrintf{logger}
	gp.Tr = p.tr
	// Forward the client's Accept-Encoding untouched: byte counts then match
	// the wire and the client gets the encoding it asked for.
	gp.KeepAcceptEncoding = true
	gp.ConnectDial = nil
	gp.ConnectDialWithReq = p.connectDial
	gp.NonproxyHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "ricochet is an HTTP proxy: use it with curl -x / HTTPS_PROXY", http.StatusBadRequest)
	})
	p.gp = gp
	p.installRoundTripper()
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

// connectDial opens the tunnel for a CONNECT request and records it.
func (p *Proxy) connectDial(req *http.Request, network, addr string) (net.Conn, error) {
	ctx := req.Context()
	start := time.Now()
	route, err := p.router.Route(addr)
	if err != nil {
		return nil, err
	}
	host, port := splitPort(addr)
	ev := p.record(collector.Event{
		StartedAt:  start,
		ClientAddr: req.RemoteAddr,
		Scheme:     collector.SchemeHTTPSTunnel,
		Host:       host,
		Port:       port,
		Route:      route.String(),
		RuleMatch:  ruleText(route),
	})
	p.log.Debug("route", "scheme", "connect", "host", addr, "route", route.String(), "rule", ruleText(route))

	fail := func(err error) (net.Conn, error) {
		ev.Err = err.Error()
		ev.Total = time.Since(start)
		ev.Done = true
		p.update(ev)
		p.log.Warn("tunnel failed", "host", addr, "route", route.String(), "err", err)
		return nil, err
	}

	conn, err := p.traceDial(ctx, network, route.Addr, &ev)
	if err != nil {
		return fail(fmt.Errorf("dial %s (%s): %w", addr, route, err))
	}
	if route.Kind == RouteUpstream {
		hs := time.Now()
		conn, err = connectThrough(ctx, conn, addr, p.auth)
		ev.Blocked = time.Since(hs)
		if err != nil {
			return fail(err)
		}
	}
	p.update(ev)
	return newTunnelConn(p, conn, start, ev), nil
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
