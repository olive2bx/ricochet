package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/olive2bx/ricochet/internal/collector"
	"github.com/olive2bx/ricochet/internal/dashboard"
)

// fakeInstance serves a dashboard whose shutdown closes it, plus a listener
// standing for the proxy port.
func fakeInstance(t *testing.T) (dashAddr string, proxyLn net.Listener) {
	t.Helper()
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var srv *httptest.Server
	srv = httptest.NewServer(dashboard.New(dashboard.Options{
		Collector: collector.New(1),
		Version:   "test",
		Status:    map[string]any{"proxy": proxyLn.Addr().String(), "upstream": "corp:8080"},
		Shutdown: func() {
			time.Sleep(50 * time.Millisecond)
			srv.CloseClientConnections()
			srv.Listener.Close()
			proxyLn.Close()
		},
	}))
	t.Cleanup(func() { srv.Close(); proxyLn.Close() })
	return strings.TrimPrefix(srv.URL, "http://"), proxyLn
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestPing(t *testing.T) {
	addr, proxyLn := fakeInstance(t)
	var out bytes.Buffer
	if code := cmdPing(addr, &out); code != ctlOK {
		t.Fatalf("ping running instance: code %d", code)
	}
	for _, want := range []string{"pid=", "version=test", "proxy=" + proxyLn.Addr().String(), "upstream=corp:8080"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("ping output %q lacks %q", out.String(), want)
		}
	}

	out.Reset()
	if code := cmdPing(freeAddr(t), &out); code != ctlNotRunning || out.Len() != 0 {
		t.Errorf("ping without instance: code %d, output %q", code, out.String())
	}
}

func TestPingIgnoresOtherServers(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"app":"something-else","pid":1}`))
	}))
	defer other.Close()
	if code := cmdPing(strings.TrimPrefix(other.URL, "http://"), &bytes.Buffer{}); code != ctlNotRunning {
		t.Errorf("ping on a non-ricochet server: code %d", code)
	}
}

func TestPingIgnoresProxyEnv(t *testing.T) {
	addr, _ := fakeInstance(t)
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	if code := cmdPing(addr, &bytes.Buffer{}); code != ctlOK {
		t.Errorf("ping through proxy env: code %d", code)
	}
}

func TestStop(t *testing.T) {
	addr, _ := fakeInstance(t)
	var out bytes.Buffer
	if code := cmdStop(addr, &out, 5*time.Second); code != ctlOK {
		t.Fatalf("stop: code %d, output %q", code, out.String())
	}
	if !strings.HasPrefix(out.String(), "pid=") {
		t.Errorf("stop output %q", out.String())
	}
	if code := cmdPing(addr, &bytes.Buffer{}); code != ctlNotRunning {
		t.Errorf("instance still answers after stop")
	}
	if code := cmdStop(addr, &bytes.Buffer{}, time.Second); code != ctlNotRunning {
		t.Errorf("second stop: code %d", code)
	}
}
