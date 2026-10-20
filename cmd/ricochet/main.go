// Command ricochet is a local HTTP/HTTPS debugging proxy: it forces some
// FQDNs to fixed IPs and chains everything else to an upstream proxy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/olive2bx/ricochet/internal/proxy"
	"github.com/olive2bx/ricochet/internal/rules"
)

type options struct {
	listen       string
	rulesPath    string
	upstream     string
	upstreamAuth string
	bypass       string
	verbose      bool
}

func main() {
	var o options
	flag.StringVar(&o.listen, "listen", "127.0.0.1:8080", "proxy listen address")
	flag.StringVar(&o.rulesPath, "rules", "", "FQDN → IP rules file (hosts-like format)")
	flag.StringVar(&o.upstream, "upstream", "", "upstream HTTP proxy, e.g. http://host:3128 (default: direct)")
	flag.StringVar(&o.upstreamAuth, "upstream-auth", "", "upstream credentials user:pass (or env RICOCHET_UPSTREAM_AUTH)")
	flag.StringVar(&o.bypass, "bypass", "", "comma-separated hosts that skip the upstream, e.g. a.com,*.b.com")
	flag.BoolVar(&o.verbose, "verbose", false, "verbose logs")
	flag.Parse()

	if o.upstreamAuth == "" {
		o.upstreamAuth = os.Getenv("RICOCHET_UPSTREAM_AUTH")
	}

	level := slog.LevelInfo
	if o.verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, o, logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o options, logger *slog.Logger) error {
	store, err := rules.NewStore(o.rulesPath)
	if err != nil {
		return fmt.Errorf("load rules: %w", err)
	}

	upstream, auth, err := parseUpstream(o.upstream, o.upstreamAuth)
	if err != nil {
		return err
	}

	p, err := proxy.New(proxy.Config{
		Rules:        store,
		Upstream:     upstream,
		UpstreamAuth: auth,
		Bypass:       splitList(o.bypass),
		Logger:       logger,
	})
	if err != nil {
		return fmt.Errorf("proxy: %w", err)
	}

	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if !isLoopback(ln.Addr()) {
		logger.Warn("proxy is listening on a non-loopback address: traffic may be exposed", "addr", ln.Addr().String())
	}

	upstreamDesc := "none (direct)"
	if upstream != nil {
		upstreamDesc = upstream.Host
	}
	logger.Info("ricochet started",
		"listen", ln.Addr().String(),
		"rules", store.Path(),
		"rule_count", len(store.Status().Rules),
		"upstream", upstreamDesc,
		"upstream_auth", auth != "",
	)
	for _, r := range store.Status().Rules {
		logger.Debug("rule", "pattern", r.Pattern, "line", r.String())
	}
	logger.Info("usage: curl -x http://" + ln.Addr().String() + " https://example.com/")

	srv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("shutdown: %w", err)
	}
	p.CloseIdleConnections()
	return nil
}

// parseUpstream parses the --upstream value. Credentials embedded in the URL
// are used when auth is empty, and always stripped from the returned URL.
func parseUpstream(raw, auth string) (*url.URL, string, error) {
	if raw == "" {
		return nil, auth, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		// url errors echo the input, which may contain credentials.
		return nil, "", errors.New("invalid --upstream URL")
	}
	if u.Scheme != "http" {
		return nil, "", fmt.Errorf("invalid --upstream: scheme %q not supported, use http://", u.Scheme)
	}
	if u.Host == "" {
		return nil, "", errors.New("invalid --upstream: missing host")
	}
	if u.User != nil && auth == "" {
		pass, _ := u.User.Password()
		auth = u.User.Username() + ":" + pass
	}
	return &url.URL{Scheme: "http", Host: u.Host}, auth, nil
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func isLoopback(a net.Addr) bool {
	ap, err := netip.ParseAddrPort(a.String())
	return err == nil && ap.Addr().IsLoopback()
}
