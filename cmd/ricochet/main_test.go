package main

import (
	"strings"
	"testing"
)

func TestParseUpstream(t *testing.T) {
	tests := []struct {
		raw, auth string
		wantHost  string
		wantAuth  string
		wantErr   bool
	}{
		{"", "", "", "", false},
		{"http://proxy:3128", "", "proxy:3128", "", false},
		{"proxy:3128", "u:p", "proxy:3128", "u:p", false},
		{"http://u:s3cr%40t@proxy:3128", "", "proxy:3128", "u:s3cr@t", false},
		{"http://u:url@proxy:3128", "flag:wins", "proxy:3128", "flag:wins", false},
		{"https://proxy:3128", "", "", "", true},
		{"http://", "", "", "", true},
		{"http://u:sec ret@proxy:%zz", "", "", "", true},
	}
	for _, tt := range tests {
		u, auth, err := parseUpstream(tt.raw, tt.auth)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseUpstream(%q) succeeded", tt.raw)
			} else if strings.Contains(err.Error(), "sec") {
				t.Errorf("error leaks credentials: %v", err)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseUpstream(%q): %v", tt.raw, err)
			continue
		}
		host := ""
		if u != nil {
			host = u.Host
			if u.User != nil {
				t.Errorf("parseUpstream(%q) kept userinfo", tt.raw)
			}
		}
		if host != tt.wantHost || auth != tt.wantAuth {
			t.Errorf("parseUpstream(%q, %q) = %q, %q; want %q, %q", tt.raw, tt.auth, host, auth, tt.wantHost, tt.wantAuth)
		}
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" a.com, ,*.b.com,")
	if strings.Join(got, "|") != "a.com|*.b.com" {
		t.Errorf("splitList = %q", got)
	}
}
