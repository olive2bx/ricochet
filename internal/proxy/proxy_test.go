package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olive2bx/ricochet/internal/collector"
)

// seen records what a test backend received.
type seen struct {
	mu   sync.Mutex
	reqs []string
}

func (s *seen) add(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, v)
}

func (s *seen) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = nil
}

func (s *seen) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

// backend answers with its name and records "host path sni".
func backend(t *testing.T, name string, useTLS bool) (*httptest.Server, *seen) {
	t.Helper()
	rec := &seen{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sni := ""
		if r.TLS != nil {
			sni = r.TLS.ServerName
		}
		rec.add(fmt.Sprintf("%s %s %s", r.Host, r.URL.Path, sni))
		io.WriteString(w, name)
	})
	var srv *httptest.Server
	if useTLS {
		srv = httptest.NewTLSServer(h) // cert valid for example.com and 127.0.0.1
	} else {
		srv = httptest.NewServer(h)
	}
	t.Cleanup(srv.Close)
	return srv, rec
}

// fakeUpstream is an HTTP proxy that forwards every CONNECT to target and
// answers plain requests itself. It records "METHOD uri auth".
func fakeUpstream(t *testing.T, target string, rejectConnect bool) (*httptest.Server, *seen) {
	t.Helper()
	rec := &seen{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Proxy-Authorization")
		if r.Method != http.MethodConnect {
			rec.add(fmt.Sprintf("%s %s %s", r.Method, r.URL.String(), auth))
			io.WriteString(w, "from-upstream")
			return
		}
		rec.add(fmt.Sprintf("CONNECT %s %s", r.Host, auth))
		if rejectConnect {
			w.Header().Set("Proxy-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		dst, err := net.Dial("tcp", target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			dst.Close()
			return
		}
		io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() { io.Copy(dst, client); dst.Close() }()
		go func() { io.Copy(client, dst); client.Close() }()
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, rec
}

func startProxy(t *testing.T, cfg Config) *url.URL {
	t.Helper()
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	t.Cleanup(p.CloseIdleConnections)
	u, _ := url.Parse(srv.URL)
	return u
}

func client(t *testing.T, proxyURL *url.URL, tlsBackend *httptest.Server) *http.Client {
	t.Helper()
	tr := &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}
	if tlsBackend != nil {
		pool := x509.NewCertPool()
		pool.AddCert(tlsBackend.Certificate())
		tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

func get(t *testing.T, c *http.Client, u string) string {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: read body: %v", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", u, resp.StatusCode, b)
	}
	return string(b)
}

func port(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	_, p, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func wantSeen(t *testing.T, rec *seen, want ...string) {
	t.Helper()
	got := rec.all()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("received:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestHTTPOverride(t *testing.T) {
	srv, rec := backend(t, "backend", false)
	up, upRec := fakeUpstream(t, "127.0.0.1:1", false)
	upURL, _ := url.Parse(up.URL)

	proxyURL := startProxy(t, Config{
		Rules:    mustRules(t, "127.0.0.1 api.example.com\n"),
		Upstream: upURL,
	})
	c := client(t, proxyURL, nil)

	p := port(t, srv)
	if got := get(t, c, "http://api.example.com:"+p+"/hello"); got != "backend" {
		t.Errorf("body = %q", got)
	}
	wantSeen(t, rec, "api.example.com:"+p+" /hello ")
	wantSeen(t, upRec) // the upstream is ignored for overridden hosts
}

func TestHTTPSTunnelOverride(t *testing.T) {
	srv, rec := backend(t, "tls-backend", true)
	p := port(t, srv)

	tests := []struct {
		name  string
		rules string
		url   string
		host  string
	}{
		{"keep port", "127.0.0.1 example.com\n", "https://example.com:" + p + "/a", "example.com:" + p},
		{"port override", "127.0.0.1:" + p + " example.com\n", "https://example.com/b", "example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec.reset()
			proxyURL := startProxy(t, Config{Rules: mustRules(t, tt.rules)})
			c := client(t, proxyURL, srv)
			if got := get(t, c, tt.url); got != "tls-backend" {
				t.Errorf("body = %q", got)
			}
			path := strings.TrimPrefix(tt.url, "https://"+tt.host)
			// Host header and SNI keep the requested name.
			wantSeen(t, rec, tt.host+" "+path+" example.com")
		})
	}
}

func TestUpstreamChaining(t *testing.T) {
	tlsSrv, tlsRec := backend(t, "tls-backend", true)
	up, upRec := fakeUpstream(t, tlsSrv.Listener.Addr().String(), false)
	// Credentials in the URL are ignored; UpstreamAuth is used.
	upURL, _ := url.Parse(strings.Replace(up.URL, "http://", "http://leak:me@", 1))

	const auth = "user:p@ss:word"
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(auth))

	px, err := New(Config{Upstream: upURL, UpstreamAuth: auth})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := px.Router().Route("nowhere.test:80")
	if s := r.String(); strings.Contains(s, "leak") || strings.Contains(s, "user") {
		t.Errorf("route %q leaks credentials", s)
	}

	proxyURL := startProxy(t, Config{Upstream: upURL, UpstreamAuth: auth})
	c := client(t, proxyURL, tlsSrv)

	if got := get(t, c, "http://nowhere.test/x?y=1"); got != "from-upstream" {
		t.Errorf("http body = %q", got)
	}
	p := port(t, tlsSrv)
	if got := get(t, c, "https://example.com:"+p+"/z"); got != "tls-backend" {
		t.Errorf("https body = %q", got)
	}
	wantSeen(t, upRec,
		"GET http://nowhere.test/x?y=1 "+wantAuth,
		"CONNECT example.com:"+p+" "+wantAuth,
	)
	wantSeen(t, tlsRec, "example.com:"+p+" /z example.com")
}

func TestUpstreamConnectRejected(t *testing.T) {
	up, upRec := fakeUpstream(t, "127.0.0.1:1", true)
	upURL, _ := url.Parse(up.URL)
	proxyURL := startProxy(t, Config{Upstream: upURL})
	c := client(t, proxyURL, nil)

	resp, err := c.Get("https://denied.test/")
	if err == nil {
		resp.Body.Close()
		t.Fatal("GET through rejecting upstream succeeded")
	}
	wantSeen(t, upRec, "CONNECT denied.test:443 ")
}

func TestBypassSkipsUpstream(t *testing.T) {
	srv, rec := backend(t, "backend", false)
	up, upRec := fakeUpstream(t, "127.0.0.1:1", false)
	upURL, _ := url.Parse(up.URL)
	proxyURL := startProxy(t, Config{Upstream: upURL, Bypass: []string{"localhost"}})
	c := client(t, proxyURL, nil)

	p := port(t, srv)
	if got := get(t, c, "http://localhost:"+p+"/direct"); got != "backend" {
		t.Errorf("body = %q", got)
	}
	wantSeen(t, rec, "localhost:"+p+" /direct ")
	wantSeen(t, upRec)
}

func TestNewRejectsBadUpstream(t *testing.T) {
	for _, raw := range []string{"https://proxy:3128", "socks5://proxy:1080", "http://"} {
		u, _ := url.Parse(raw)
		if _, err := New(Config{Upstream: u}); err == nil {
			t.Errorf("New(%q) succeeded", raw)
		}
	}
}

// waitDone polls col until n events are done.
func waitDone(t *testing.T, col *collector.Collector, n int) []collector.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs := col.All()
		done := 0
		for _, e := range evs {
			if e.Done {
				done++
			}
		}
		if done >= n {
			return evs
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %d done events, got %+v", n, evs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEventsHTTP(t *testing.T) {
	srv, _ := backend(t, "backend", false)
	col := collector.New(10)
	proxyURL := startProxy(t, Config{
		Rules:     mustRules(t, "127.0.0.1 api.example.com\n"),
		Collector: col,
	})
	c := client(t, proxyURL, nil)

	p := port(t, srv)
	req, _ := http.NewRequest(http.MethodPost, "http://api.example.com:"+p+"/submit?q=1", strings.NewReader("hello=world"))
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set("X-Trace", "abc")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	e := waitDone(t, col, 1)[0]
	if e.Scheme != collector.SchemeHTTP || e.Method != "POST" || e.Path != "/submit?q=1" || e.Status != 200 ||
		e.Host != "api.example.com" || p != strconv.Itoa(int(e.Port)) {
		t.Errorf("event = %+v", e)
	}
	if e.Route != "override:127.0.0.1" || !strings.Contains(e.RuleMatch, "api.example.com") {
		t.Errorf("route = %q, rule = %q", e.Route, e.RuleMatch)
	}
	if e.BytesUp != int64(len("hello=world")) || e.BytesDown != int64(len("backend")) {
		t.Errorf("bytes up/down = %d/%d", e.BytesUp, e.BytesDown)
	}
	if e.Total <= 0 || e.TTFB <= 0 || e.Connect <= 0 || e.TTFB > e.Total {
		t.Errorf("timings total=%v ttfb=%v connect=%v", e.Total, e.TTFB, e.Connect)
	}
	if e.DNS != 0 {
		t.Errorf("DNS = %v for an IP override", e.DNS)
	}
	if got := e.ReqHeader.Get("Authorization"); got != "[masked]" {
		t.Errorf("Authorization = %q, want masked", got)
	}
	if got := e.ReqHeader.Get("X-Trace"); got != "abc" {
		t.Errorf("X-Trace = %q", got)
	}
	if e.RespHeader.Get("Content-Type") == "" {
		t.Errorf("response headers missing: %v", e.RespHeader)
	}
	if e.ReqBody != nil || e.RespBody != nil {
		t.Error("bodies captured without CaptureBodies")
	}
}

func TestEventsTunnel(t *testing.T) {
	srv, _ := backend(t, "tls-backend", true)
	col := collector.New(10)
	proxyURL := startProxy(t, Config{Rules: mustRules(t, "127.0.0.1 example.com\n"), Collector: col})
	c := client(t, proxyURL, srv)

	p := port(t, srv)
	get(t, c, "https://example.com:"+p+"/")
	c.CloseIdleConnections()

	e := waitDone(t, col, 1)[0]
	if e.Scheme != collector.SchemeHTTPSTunnel || e.Host != "example.com" || e.Method != "" || e.Status != 0 {
		t.Errorf("event = %+v", e)
	}
	if e.Route != "override:127.0.0.1" {
		t.Errorf("route = %q", e.Route)
	}
	if e.BytesUp < 100 || e.BytesDown < 100 {
		t.Errorf("bytes up/down = %d/%d", e.BytesUp, e.BytesDown)
	}
	if e.Connect <= 0 || e.TTFB <= 0 || e.Total < e.TTFB {
		t.Errorf("timings connect=%v ttfb=%v total=%v", e.Connect, e.TTFB, e.Total)
	}
	if e.Err != "" {
		t.Errorf("err = %q", e.Err)
	}
}

func TestEventsTunnelError(t *testing.T) {
	up, _ := fakeUpstream(t, "127.0.0.1:1", true)
	upURL, _ := url.Parse(up.URL)
	col := collector.New(10)
	proxyURL := startProxy(t, Config{Upstream: upURL, Collector: col})
	c := client(t, proxyURL, nil)
	if resp, err := c.Get("https://denied.test/"); err == nil {
		resp.Body.Close()
	}
	e := waitDone(t, col, 1)[0]
	if !strings.Contains(e.Err, "407") || !strings.HasPrefix(e.Route, "upstream:") {
		t.Errorf("event = %+v", e)
	}
}
