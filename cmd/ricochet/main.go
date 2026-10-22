// Command ricochet is a local HTTP/HTTPS debugging proxy: it forces some
// FQDNs to fixed IPs and chains everything else to an upstream proxy.
package main

import (
	"context"
	"crypto/tls"
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/olive2bx/ricochet/internal/collector"
	"github.com/olive2bx/ricochet/internal/dashboard"
	"github.com/olive2bx/ricochet/internal/history"
	"github.com/olive2bx/ricochet/internal/proxy"
	"github.com/olive2bx/ricochet/internal/rules"
)

type options struct {
	listen       string
	rulesPath    string
	upstream     string
	upstreamAuth string
	bypass       string
	dashboard    string
	history      int
	noKeepAlive  bool
	mitm         bool
	mitmHosts    string
	caDir        string
	serverCA     string
	insecure     bool
	maskHeaders  string
	captureBody  int
	dbPath       string
	dbMaxRows    int
	verbose      bool
}

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var o options
	flag.StringVar(&o.listen, "listen", "127.0.0.1:8080", "proxy listen address")
	flag.StringVar(&o.rulesPath, "rules", "", "FQDN → IP rules file (hosts-like format)")
	flag.StringVar(&o.upstream, "upstream", "", "upstream HTTP proxy, e.g. http://host:3128 (or env RICOCHET_UPSTREAM; default: direct)")
	flag.StringVar(&o.upstreamAuth, "upstream-auth", "", "upstream credentials user:pass (or env RICOCHET_UPSTREAM_AUTH)")
	flag.StringVar(&o.bypass, "bypass", "", "comma-separated hosts that skip the upstream, e.g. a.com,*.b.com")
	flag.StringVar(&o.dashboard, "dashboard", "127.0.0.1:8081", `dashboard listen address ("" to disable)`)
	flag.IntVar(&o.history, "history", 5000, "number of requests kept in memory")
	flag.BoolVar(&o.noKeepAlive, "no-keepalive", false, "disable connection reuse towards servers (per-request DNS/connect/TLS timings)")
	flag.BoolVar(&o.mitm, "mitm", false, "decrypt (MITM) every HTTPS connection")
	flag.StringVar(&o.mitmHosts, "mitm-hosts", "", "comma-separated hosts to decrypt, e.g. *.example.com,api.foo.com")
	flag.StringVar(&o.caDir, "ca-dir", "./.ricochet-ca", "directory of the local MITM CA (created on first use)")
	flag.StringVar(&o.serverCA, "upstream-ca", "", "comma-separated PEM files of extra CAs trusted for MITM'd servers (e.g. a Kubernetes cluster CA)")
	flag.BoolVar(&o.insecure, "upstream-insecure", false, "MITM: do not verify server certificates (debugging only)")
	flag.StringVar(&o.maskHeaders, "mask-headers", strings.Join(proxy.DefaultMaskedHeaders, ","), `headers whose values are hidden in the dashboard ("" to show everything)`)
	flag.IntVar(&o.captureBody, "capture-bodies", 0, "store up to N bytes of each request/response body (plain HTTP and MITM; 0 = off)")
	flag.StringVar(&o.dbPath, "db", "", "SQLite file for a persistent history (binary built with -tags sqlite)")
	flag.IntVar(&o.dbMaxRows, "db-max-rows", history.DefaultMaxRows, "max events kept in the SQLite history")
	flag.BoolVar(&o.verbose, "verbose", false, "verbose logs")
	showVersion := flag.Bool("version", false, "print the version and exit")
	ping := flag.Bool("ping", false, "check whether a ricochet instance answers on --dashboard, print its pid and exit (0 = running, 1 = not running)")
	stopInstance := flag.Bool("stop", false, "gracefully stop the ricochet instance whose dashboard is --dashboard and exit (0 = stopped, 1 = not running)")
	flag.Parse()
	if *showVersion {
		fmt.Println("ricochet", version)
		return
	}
	if *ping {
		os.Exit(cmdPing(o.dashboard, os.Stdout))
	}
	if *stopInstance {
		os.Exit(cmdStop(o.dashboard, os.Stdout, 10*time.Second))
	}
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %v\n", flag.Args())
		flag.Usage()
		os.Exit(2)
	}

	// Environment fallbacks keep credentials out of the process list.
	if o.upstream == "" {
		o.upstream = os.Getenv("RICOCHET_UPSTREAM")
	}
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
	// Cancelled by SIGINT/SIGTERM or by POST /api/shutdown (ricochet --stop).
	ctx, shutdown := context.WithCancel(ctx)
	defer shutdown()

	store, err := rules.NewStore(o.rulesPath)
	if err != nil {
		return fmt.Errorf("load rules: %w", err)
	}

	upstream, auth, err := parseUpstream(o.upstream, o.upstreamAuth)
	if err != nil {
		return err
	}

	col := collector.New(o.history)
	if o.dbPath != "" {
		db, err := history.Open(o.dbPath, o.dbMaxRows, logger)
		if err != nil {
			return err
		}
		defer db.Close()
		past, err := db.Load(o.history)
		if err != nil {
			return err
		}
		col.Load(past)
		col.SetSink(db)
		logger.Info("persistent history", "db", o.dbPath, "loaded", len(past))
	}

	var ca *tls.Certificate
	mitmHosts := splitList(o.mitmHosts)
	caPath := ""
	if o.mitm || len(mitmHosts) > 0 {
		var created bool
		ca, caPath, created, err = proxy.LoadOrCreateCA(o.caDir)
		if err != nil {
			return fmt.Errorf("MITM CA: %w", err)
		}
		if abs, err := filepath.Abs(caPath); err == nil {
			caPath = abs
		}
		if created {
			logger.Info("generated a new local MITM CA (not installed anywhere)", "cert", caPath)
		}
	}
	mitmScope := ""
	switch {
	case o.mitm:
		mitmScope = "all"
	case len(mitmHosts) > 0:
		mitmScope = strings.Join(mitmHosts, ",")
	}
	mask := splitList(o.maskHeaders)
	if mask == nil {
		mask = []string{}
	}

	serverTLS, err := proxy.ServerTLSConfig(splitList(o.serverCA), o.insecure)
	if err != nil {
		return err
	}
	if o.insecure {
		logger.Warn("MITM: server certificates are NOT verified (--upstream-insecure)")
	}

	p, err := proxy.New(proxy.Config{
		Rules:         store,
		Upstream:      upstream,
		UpstreamAuth:  auth,
		Bypass:        splitList(o.bypass),
		Logger:        logger,
		Collector:     col,
		NoKeepAlive:   o.noKeepAlive,
		MaskHeaders:   mask,
		CA:            ca,
		MITM:          o.mitm,
		MITMHosts:     mitmHosts,
		CaptureBodies: o.captureBody,

		TLSClientConfig: serverTLS,
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
	go store.Watch(ctx, logger, func(err error) {
		if err != nil {
			logger.Error("rules reload failed, keeping the previous rules", "err", err)
			return
		}
		// Pooled connections may point to the old addresses.
		p.CloseIdleConnections()
		logger.Info("rules reloaded", "rule_count", len(store.Status().Rules))
	})

	logger.Info("ricochet started",
		"version", version,
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
	if ca != nil {
		scope := "all hosts"
		if !o.mitm {
			scope = strings.Join(mitmHosts, ",")
		}
		logger.Info("MITM enabled", "hosts", scope, "ca", caPath)
		logger.Info(fmt.Sprintf("trust the CA per command, e.g.: SSL_CERT_FILE=%s curl -x http://%s https://...  (or curl --cacert %s)", caPath, ln.Addr(), caPath))
	}

	srv := &http.Server{
		Handler:           p,
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}
	errc := make(chan error, 2)
	go func() { errc <- srv.Serve(ln) }()

	var dash *http.Server
	if o.dashboard != "" {
		dln, err := net.Listen("tcp", o.dashboard)
		if err != nil {
			ln.Close()
			return fmt.Errorf("dashboard listen: %w", err)
		}
		var allowed []string
		if !isLoopback(dln.Addr()) {
			logger.Warn("dashboard is listening on a non-loopback address: captured traffic may be exposed", "addr", dln.Addr().String())
			if h, _, err := net.SplitHostPort(o.dashboard); err == nil && h != "" {
				allowed = append(allowed, h)
			}
			if hn, err := os.Hostname(); err == nil {
				allowed = append(allowed, hn)
			}
		}
		info := map[string]string{"proxy": ln.Addr().String(), "amont": upstreamDesc}
		if o.captureBody > 0 {
			info["corps"] = fmt.Sprintf("≤ %d o", o.captureBody)
		}
		switch {
		case o.mitm:
			info["mitm"] = "tous les hosts"
		case len(mitmHosts) > 0:
			info["mitm"] = strings.Join(mitmHosts, ",")
		}
		switch {
		case o.insecure:
			info["certificats serveurs"] = "NON vérifiés (--upstream-insecure)"
		case o.serverCA != "":
			info["certificats serveurs"] = "système + " + o.serverCA
		}
		dash = &http.Server{
			Handler: dashboard.New(dashboard.Options{
				Collector:    col,
				Rules:        store.Status,
				Info:         info,
				Logger:       logger,
				AllowedHosts: allowed,
				Version:      version,
				Status: map[string]any{
					"proxy": ln.Addr().String(), "upstream": upstreamDesc,
					"mitm": mitmScope, "ca": caPath,
				},
				Shutdown: shutdown,
			}),
			ReadHeaderTimeout: 10 * time.Second,
			ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
			// Cancelled on shutdown so that SSE streams end.
			BaseContext: func(net.Listener) context.Context { return ctx },
		}
		go func() { errc <- dash.Serve(dln) }()
		logger.Info("dashboard: http://" + dln.Addr().String() + "/")
	}

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if dash != nil {
		if err := dash.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			logger.Warn("dashboard shutdown", "err", err)
		}
	}
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
