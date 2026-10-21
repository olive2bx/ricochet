package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/olive2bx/ricochet/internal/collector"
)

func TestLoadOrCreateCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	ca, certPath, created, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created || certPath != filepath.Join(dir, CACertFile) || !ca.Leaf.IsCA {
		t.Fatalf("created=%v path=%q isCA=%v", created, certPath, ca.Leaf.IsCA)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(dir, CAKeyFile))
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Errorf("key permissions = %o, want 600", perm)
		}
	}

	again, _, created, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if created || !again.Leaf.Equal(ca.Leaf) {
		t.Error("CA regenerated instead of reloaded")
	}

	if err := os.Remove(filepath.Join(dir, CAKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := LoadOrCreateCA(dir); err == nil {
		t.Error("loading a CA without key succeeded")
	}
}

// mitmSetup returns a client trusting the ricochet CA, going through a
// proxy that trusts the TLS backend.
func mitmSetup(t *testing.T, cfg Config, tlsBackend interface{ Certificate() *x509.Certificate }) (*http.Client, *collector.Collector) {
	t.Helper()
	ca, certPath, _, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	backendPool := x509.NewCertPool()
	backendPool.AddCert(tlsBackend.Certificate())
	col := collector.New(20)
	cfg.CA = ca
	cfg.Collector = col
	cfg.TLSClientConfig = &tls.Config{RootCAs: backendPool}
	proxyURL := startProxy(t, cfg)

	pem, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	clientPool := x509.NewCertPool()
	clientPool.AppendCertsFromPEM(pem)
	clientPool.AddCert(tlsBackend.Certificate()) // for non-MITM'd tunnels
	tr := &http.Transport{
		Proxy:             http.ProxyURL(proxyURL),
		TLSClientConfig:   &tls.Config{RootCAs: clientPool},
		DisableKeepAlives: true,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}, col
}

func TestMITM(t *testing.T) {
	srv, rec := backend(t, "tls-backend", true)
	p := port(t, srv)
	c, col := mitmSetup(t, Config{
		Rules:     mustRules(t, "127.0.0.1 example.com\n"),
		MITMHosts: []string{"example.com"},
	}, srv)

	req, _ := http.NewRequest(http.MethodGet, "https://example.com:"+p+"/slow/secret?x=1", nil)
	req.Header.Set("Cookie", "session=42")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET via MITM: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "tls-backend" {
		t.Fatalf("body = %q", body)
	}
	// The client saw a certificate issued by the ricochet CA.
	if issuer := resp.TLS.PeerCertificates[0].Issuer.CommonName; !strings.Contains(issuer, "ricochet") {
		t.Errorf("client certificate issuer = %q", issuer)
	}
	// The backend still sees the original Host and SNI.
	wantSeen(t, rec, "example.com:"+p+" /slow/secret example.com")

	evs := waitDone(t, col, 1)
	if len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
	e := evs[0]
	if e.Scheme != collector.SchemeHTTPSMITM || e.Method != "GET" || e.Path != "/slow/secret?x=1" || e.Status != 200 {
		t.Errorf("event = %+v", e)
	}
	if e.Route != "override:127.0.0.1" {
		t.Errorf("route = %q", e.Route)
	}
	if e.TLS <= 0 || e.Connect < 0 || e.TTFB < slowDelay {
		t.Errorf("timings tls=%v connect=%v ttfb=%v", e.TLS, e.Connect, e.TTFB)
	}
	if e.ReqHeader.Get("Cookie") != "[masked]" {
		t.Errorf("Cookie not masked: %v", e.ReqHeader)
	}
}

func TestMITMHostsFilter(t *testing.T) {
	srv, _ := backend(t, "tls-backend", true)
	p := port(t, srv)
	c, col := mitmSetup(t, Config{
		Rules:     mustRules(t, "127.0.0.1 example.com\n"),
		MITMHosts: []string{"*.other.test"},
	}, srv)

	resp, err := c.Get("https://example.com:" + p + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if issuer := resp.TLS.PeerCertificates[0].Issuer.CommonName; strings.Contains(issuer, "ricochet") {
		t.Errorf("host not in --mitm-hosts was intercepted")
	}
	e := waitDone(t, col, 1)[0]
	if e.Scheme != collector.SchemeHTTPSTunnel {
		t.Errorf("scheme = %q, want tunnel", e.Scheme)
	}
}

func TestMITMThroughUpstream(t *testing.T) {
	srv, rec := backend(t, "tls-backend", true)
	up, upRec := fakeUpstream(t, srv.Listener.Addr().String(), false)
	upURL, _ := url.Parse(up.URL)
	p := port(t, srv)
	c, col := mitmSetup(t, Config{Upstream: upURL, UpstreamAuth: "u:p", MITM: true}, srv)

	if got := get(t, c, "https://example.com:"+p+"/via-upstream"); got != "tls-backend" {
		t.Errorf("body = %q", got)
	}
	wantSeen(t, upRec, "CONNECT example.com:"+p+" Basic dTpw")
	wantSeen(t, rec, "example.com:"+p+" /via-upstream example.com")
	e := waitDone(t, col, 1)[0]
	if e.Scheme != collector.SchemeHTTPSMITM || !strings.HasPrefix(e.Route, "upstream:") {
		t.Errorf("event = %+v", e)
	}
}

func TestMITMRequiresCA(t *testing.T) {
	if _, err := New(Config{MITM: true}); err == nil {
		t.Error("New with MITM and no CA succeeded")
	}
}
