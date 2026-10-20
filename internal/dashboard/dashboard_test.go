package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/olive2bx/ricochet/internal/collector"
	"github.com/olive2bx/ricochet/internal/rules"
)

func newTestServer(t *testing.T) (*httptest.Server, *collector.Collector) {
	t.Helper()
	col := collector.New(100)
	set, err := rules.Parse(strings.NewReader("10.0.0.1 a.example.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(Options{
		Collector: col,
		Rules: func() rules.Status {
			return rules.Status{Path: "rules.txt", Rules: set.Rules(), LoadedAt: time.Unix(1700000000, 0), LastErr: errors.New("line 3: bad")}
		},
		Info: map[string]string{"proxy": "127.0.0.1:8080"},
	}))
	t.Cleanup(srv.Close)
	return srv, col
}

func addEvent(col *collector.Collector, host string, total time.Duration) collector.Event {
	e := col.Start(collector.Event{Host: host, Scheme: collector.SchemeHTTP, Route: "direct", RespBody: []byte("body")})
	e.Total = total
	e.Done = true
	col.Update(e)
	return e
}

func getJSON(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK && v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return resp.StatusCode
}

func TestIndex(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || len(b) == 0 {
			t.Errorf("GET %s = %d (%d bytes)", path, resp.StatusCode, len(b))
		}
		if strings.Contains(string(b), `src="http`) || strings.Contains(string(b), `href="http`) ||
			strings.Contains(string(b), "@import") || strings.Contains(string(b), "url(http") {
			t.Errorf("%s references an external resource", path)
		}
	}
}

func TestRequestsAPI(t *testing.T) {
	srv, col := newTestServer(t)
	addEvent(col, "fast.example.com", 5*time.Millisecond)
	slow := addEvent(col, "slow.example.com", 800*time.Millisecond)
	addEvent(col, "other.net", 1200*time.Millisecond)

	tests := []struct {
		query string
		want  []string
	}{
		{"", []string{"fast.example.com", "slow.example.com", "other.net"}},
		{"?limit=1", []string{"other.net"}},
		{"?host=example", []string{"fast.example.com", "slow.example.com"}},
		{"?min_ms=500", []string{"slow.example.com", "other.net"}},
		{"?host=example&min_ms=500", []string{"slow.example.com"}},
	}
	for _, tt := range tests {
		var got []map[string]any
		if code := getJSON(t, srv.URL+"/api/requests"+tt.query, &got); code != 200 {
			t.Fatalf("%s: status %d", tt.query, code)
		}
		var hosts []string
		for _, e := range got {
			hosts = append(hosts, e["host"].(string))
			if _, ok := e["resp_body"]; ok {
				t.Errorf("list carries bodies")
			}
		}
		if strings.Join(hosts, ",") != strings.Join(tt.want, ",") {
			t.Errorf("%q: got %v, want %v", tt.query, hosts, tt.want)
		}
	}
	if code := getJSON(t, srv.URL+"/api/requests?min_ms=abc", nil); code != http.StatusBadRequest {
		t.Errorf("invalid min_ms: status %d", code)
	}

	var one map[string]any
	if code := getJSON(t, srv.URL+"/api/requests/"+itoa(slow.ID), &one); code != 200 {
		t.Fatalf("get one: %d", code)
	}
	if one["resp_body"] != "Ym9keQ==" || one["total_ms"] != 800.0 {
		t.Errorf("detail = %v", one)
	}
	if code := getJSON(t, srv.URL+"/api/requests/999", nil); code != http.StatusNotFound {
		t.Errorf("missing id: status %d", code)
	}
}

func itoa(n uint64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestRulesAPI(t *testing.T) {
	srv, _ := newTestServer(t)
	var got struct {
		Path     string `json:"path"`
		LoadedAt string `json:"loaded_at"`
		LastErr  string `json:"last_error"`
		Rules    []struct {
			Pattern, IP, Text string
		} `json:"rules"`
	}
	getJSON(t, srv.URL+"/api/rules", &got)
	if got.Path != "rules.txt" || got.LoadedAt == "" || got.LastErr != "line 3: bad" ||
		len(got.Rules) != 1 || got.Rules[0].IP != "10.0.0.1" || got.Rules[0].Pattern != "a.example.com" {
		t.Errorf("rules = %+v", got)
	}
}

func TestClear(t *testing.T) {
	srv, col := newTestServer(t)
	addEvent(col, "a", 0)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/clear", nil)
	req.Header.Set("Origin", "http://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin clear: status %d", resp.StatusCode)
	}
	if n := len(col.List(collector.Filter{})); n != 1 {
		t.Fatalf("history cleared by cross-origin request")
	}

	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/api/clear", nil)
	req.Header.Set("Origin", srv.URL)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("clear: status %d", resp.StatusCode)
	}
	if n := len(col.List(collector.Filter{})); n != 0 {
		t.Errorf("%d events after clear", n)
	}
	if code := getJSON(t, srv.URL+"/api/clear", nil); code == http.StatusNoContent || code == http.StatusOK {
		t.Errorf("GET /api/clear: status %d", code)
	}
}

func TestHostCheck(t *testing.T) {
	srv, _ := newTestServer(t)
	for host, want := range map[string]int{
		"localhost:8081":    200,
		"127.0.0.1:8081":    200,
		"[::1]:8081":        200,
		"attacker.example":  403,
		"192.168.1.10:8081": 403,
	} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/info", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("Host %q: status %d, want %d", host, resp.StatusCode, want)
		}
	}
}

func TestSSE(t *testing.T) {
	srv, col := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	r := bufio.NewReader(resp.Body)
	// Wait for the stream to be established before publishing.
	if _, err := r.ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	addEvent(col, "live.example.com", time.Millisecond)
	if err := col.Clear(); err != nil {
		t.Fatal(err)
	}

	var got []string
	for len(got) < 3 {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE: %v (got %v)", err, got)
		}
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "event: "):
			got = append(got, strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "data: {\"id\""):
			if !strings.Contains(line, "live.example.com") || strings.Contains(line, "resp_body") {
				t.Errorf("unexpected data %s", line)
			}
		}
	}
	// Start + Update + clear
	if strings.Join(got, ",") != "event,event,clear" {
		t.Errorf("SSE events = %v", got)
	}
}
