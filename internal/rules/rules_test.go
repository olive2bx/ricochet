package rules

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sample = `
# comment line
10.1.2.3        api.example.com
10.1.2.4        *.internal.example.com
10.1.2.9        *.example.com
10.1.2.5:8443   svc.example.com   other.example.com   # inline comment
10.1.2.6        exact.internal.example.com
2001:db8::1     v6.example.com
[2001:db8::2]:9443 v6port.example.com
10.1.2.7        MiXeD.Example.ORG.
`

func TestLookup(t *testing.T) {
	s, err := Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tests := []struct {
		name     string
		host     string
		wantOK   bool
		wantIP   string
		wantPort uint16
		wantLine int
	}{
		{"exact", "api.example.com", true, "10.1.2.3", 0, 3},
		{"case insensitive host", "API.Example.COM", true, "10.1.2.3", 0, 3},
		{"case insensitive rule", "mixed.example.org", true, "10.1.2.7", 0, 10},
		{"trailing dot", "api.example.com.", true, "10.1.2.3", 0, 3},
		{"wildcard subdomain", "foo.internal.example.com", true, "10.1.2.4", 0, 4},
		{"wildcard deep subdomain", "a.b.internal.example.com", true, "10.1.2.4", 0, 4},
		{"most specific wildcard", "foo.example.com", true, "10.1.2.9", 0, 5},
		{"wildcard excludes bare domain", "internal.example.com", true, "10.1.2.9", 0, 5},
		{"exact beats wildcard", "exact.internal.example.com", true, "10.1.2.6", 0, 7},
		{"port override", "svc.example.com", true, "10.1.2.5", 8443, 6},
		{"multiple hosts per line", "other.example.com", true, "10.1.2.5", 8443, 6},
		{"ipv6", "v6.example.com", true, "2001:db8::1", 0, 8},
		{"ipv6 with port", "v6port.example.com", true, "2001:db8::2", 9443, 9},
		{"no match", "example.net", false, "", 0, 0},
		{"bare domain without rule", "example.com", false, "", 0, 0},
		{"suffix is not a subdomain", "fooexample.com", false, "", 0, 0},
		{"empty", "", false, "", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, ok := s.Lookup(tt.host)
			if ok != tt.wantOK {
				t.Fatalf("Lookup(%q) ok = %v, want %v", tt.host, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got := r.Addr.String(); got != tt.wantIP {
				t.Errorf("ip = %s, want %s", got, tt.wantIP)
			}
			if r.Port != tt.wantPort {
				t.Errorf("port = %d, want %d", r.Port, tt.wantPort)
			}
			if r.Line != tt.wantLine {
				t.Errorf("line = %d, want %d", r.Line, tt.wantLine)
			}
			ip, port, ok := s.Resolve(tt.host)
			if !ok || ip != r.Addr || port != r.Port {
				t.Errorf("Resolve(%q) = %v, %d, %v; inconsistent with Lookup", tt.host, ip, port, ok)
			}
		})
	}
	if got := s.Len(); got != 9 {
		t.Errorf("Len = %d, want 9", got)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{"missing host", "10.0.0.1\n", "line 1"},
		{"bad ip", "10.0.0.300 a.com\n", "invalid address"},
		{"port zero", "10.0.0.1:0 a.com\n", "port must be"},
		{"port out of range", "10.0.0.1:70000 a.com\n", "invalid address"},
		{"hostname as target", "example.com a.com\n", "invalid address"},
		{"bad host chars", "10.0.0.1 a/b.com\n", "invalid host"},
		{"bare wildcard", "10.0.0.1 *\n", "invalid host"},
		{"wildcard in middle", "10.0.0.1 a.*.com\n", "invalid host"},
		{"empty label", "10.0.0.1 a..com\n", "invalid host"},
		{"duplicate", "10.0.0.1 a.com\n10.0.0.2 A.com\n", "duplicate host"},
		{"reports later line", "# ok\n10.0.0.1 a.com\nnope\n", "line 3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := Parse(strings.NewReader(tt.input))
			if err == nil {
				t.Fatalf("Parse succeeded with %d rules, want error", s.Len())
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseEmpty(t *testing.T) {
	s, err := Parse(strings.NewReader("\n  # only comments\n\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0", s.Len())
	}
	if _, ok := s.Lookup("a.com"); ok {
		t.Error("unexpected match on empty set")
	}
}

func TestIPv4MappedIsUnmapped(t *testing.T) {
	s, err := Parse(strings.NewReader("::ffff:10.0.0.1 a.com\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ip, _, _ := s.Resolve("a.com")
	if want := netip.MustParseAddr("10.0.0.1"); ip != want {
		t.Errorf("ip = %v, want %v", ip, want)
	}
}

func TestMatchPattern(t *testing.T) {
	tests := []struct {
		pattern, host string
		want          bool
	}{
		{"a.com", "a.com", true},
		{"a.com", "A.COM.", true},
		{"a.com", "b.a.com", false},
		{"*.a.com", "b.a.com", true},
		{"*.a.com", "c.b.a.com", true},
		{"*.a.com", "a.com", false},
		{"*.a.com", "xa.com", false},
		{"", "a.com", false},
	}
	for _, tt := range tests {
		if got := MatchPattern(tt.pattern, tt.host); got != tt.want {
			t.Errorf("MatchPattern(%q, %q) = %v, want %v", tt.pattern, tt.host, got, tt.want)
		}
	}
}

func TestStoreReloadKeepsRulesOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.txt")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("10.0.0.1 a.com\n")
	st, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if ip, _, ok := st.Resolve("a.com"); !ok || ip.String() != "10.0.0.1" {
		t.Fatalf("Resolve = %v, %v", ip, ok)
	}
	first := st.Status()
	if first.LoadedAt.IsZero() || first.LastErr != nil || len(first.Rules) != 1 {
		t.Fatalf("unexpected status %+v", first)
	}

	write("10.0.0.2 a.com\ngarbage\n")
	if err := st.Reload(); err == nil {
		t.Fatal("Reload of invalid file succeeded")
	}
	if ip, _, ok := st.Resolve("a.com"); !ok || ip.String() != "10.0.0.1" {
		t.Errorf("rules changed after invalid reload: %v, %v", ip, ok)
	}
	if s := st.Status(); s.LastErr == nil || !s.LoadedAt.Equal(first.LoadedAt) {
		t.Errorf("status after failed reload = %+v", s)
	}

	write("10.0.0.3 b.com\n")
	if err := st.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, _, ok := st.Resolve("a.com"); ok {
		t.Error("a.com still matches after reload")
	}
	if ip, _, ok := st.Resolve("b.com"); !ok || ip.String() != "10.0.0.3" {
		t.Errorf("Resolve(b.com) = %v, %v", ip, ok)
	}
	if s := st.Status(); s.LastErr != nil {
		t.Errorf("LastErr = %v after successful reload", s.LastErr)
	}
}

func TestNewStore(t *testing.T) {
	st, err := NewStore("")
	if err != nil {
		t.Fatalf("NewStore(\"\"): %v", err)
	}
	if _, _, ok := st.Resolve("a.com"); ok {
		t.Error("empty store matched")
	}
	if _, err := NewStore(filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Error("NewStore on missing file succeeded")
	}
	bad := filepath.Join(t.TempDir(), "bad.txt")
	if err := os.WriteFile(bad, []byte("nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(bad); err == nil {
		t.Error("NewStore on invalid file succeeded")
	}
}

func waitFor(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func resolvesTo(st *Store, host, ip string) func() bool {
	return func() bool {
		got, _, ok := st.Resolve(host)
		return ok && got.String() == ip
	}
}

func TestWatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.txt")
	if err := os.WriteFile(path, []byte("10.0.0.1 a.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reloads := make(chan error, 10)
	go st.Watch(ctx, nil, func(err error) { reloads <- err })
	time.Sleep(100 * time.Millisecond) // let the watcher start

	// In-place write.
	if err := os.WriteFile(path, []byte("10.0.0.2 a.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, resolvesTo(st, "a.com", "10.0.0.2"))

	// Invalid content keeps the current rules and reports the error.
	if err := os.WriteFile(path, []byte("10.0.0.3 a.com\nbroken line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return st.Status().LastErr != nil })
	if !resolvesTo(st, "a.com", "10.0.0.2")() {
		t.Error("rules changed after an invalid edit")
	}

	// Atomic save: write a temp file then rename it over the original.
	tmp := filepath.Join(dir, ".rules.txt.tmp")
	if err := os.WriteFile(tmp, []byte("10.0.0.4 a.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, resolvesTo(st, "a.com", "10.0.0.4"))
	if st.Status().LastErr != nil {
		t.Errorf("LastErr = %v after a valid reload", st.Status().LastErr)
	}
}

func TestPoll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.txt")
	if err := os.WriteFile(path, []byte("10.0.0.1 a.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go st.poll(ctx, 20*time.Millisecond, func() { st.Reload() })
	time.Sleep(50 * time.Millisecond)
	// Different size so that coarse mtime resolution does not matter.
	if err := os.WriteFile(path, []byte("10.0.0.22 a.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, resolvesTo(st, "a.com", "10.0.0.22"))
}
