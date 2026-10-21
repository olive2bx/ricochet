// Package rules parses and matches the FQDN → IP override file.
//
// The format is inspired by /etc/hosts:
//
//	# comment
//	10.1.2.3        api.example.com
//	10.1.2.4        *.internal.example.com
//	10.1.2.5:8443   svc.example.com other.example.com   # port override
//	[2001:db8::1]:443 v6.example.com
//
// Exact names take precedence over wildcards; "*.domain" matches any
// subdomain of domain (at any depth) but not domain itself. When several
// wildcards match, the most specific one wins. Matching is case-insensitive.
package rules

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
)

// Rule is a single host → address mapping.
type Rule struct {
	Pattern string     // normalized host or "*.domain"
	Addr    netip.Addr // forced destination IP
	Port    uint16     // forced destination port, 0 = keep the original one
	Line    int        // 1-based line number in the source file
	Text    string     // source line, comment stripped and trimmed
}

// String returns the source line of the rule, prefixed by its line number.
func (r Rule) String() string {
	return fmt.Sprintf("%d: %s", r.Line, r.Text)
}

// Set is an immutable collection of parsed rules.
type Set struct {
	exact map[string]Rule
	wild  map[string]Rule // keyed by suffix without the "*." prefix
	all   []Rule
}

// Lookup returns the rule applying to host. host may carry a trailing dot,
// mixed case, or IPv6 brackets; it must not include a port.
func (s *Set) Lookup(host string) (Rule, bool) {
	if s == nil {
		return Rule{}, false
	}
	h := normalizeHost(host)
	if h == "" {
		return Rule{}, false
	}
	if r, ok := s.exact[h]; ok {
		return r, true
	}
	// Walk parent domains from the most specific to the least specific.
	for suffix := h; ; {
		i := strings.IndexByte(suffix, '.')
		if i < 0 {
			return Rule{}, false
		}
		suffix = suffix[i+1:]
		if r, ok := s.wild[suffix]; ok {
			return r, true
		}
	}
}

// Resolve returns the forced IP and port for host. port is 0 when the
// original destination port must be kept.
func (s *Set) Resolve(host string) (ip netip.Addr, port uint16, ok bool) {
	r, ok := s.Lookup(host)
	if !ok {
		return netip.Addr{}, 0, false
	}
	return r.Addr, r.Port, true
}

// Rules returns the rules in file order.
func (s *Set) Rules() []Rule {
	if s == nil {
		return nil
	}
	out := make([]Rule, len(s.all))
	copy(out, s.all)
	return out
}

// Len returns the number of rules.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.all)
}

// ParseFile parses the rules file at path.
func ParseFile(path string) (*Set, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open rules: %w", err)
	}
	defer f.Close()
	s, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Parse reads rules from r. All syntax errors are reported, joined; on error
// no Set is returned.
func Parse(r io.Reader) (*Set, error) {
	s := &Set{exact: map[string]Rule{}, wild: map[string]Rule{}}
	seen := map[string]int{}
	var errs []error

	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if n == 1 {
			// Windows editors often save UTF-8 with a byte order mark.
			line = strings.TrimPrefix(line, "\uFEFF")
		}
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			errs = append(errs, fmt.Errorf("line %d: expected \"<ip>[:port] <host>...\", got %q", n, line))
			continue
		}
		addr, port, err := parseTarget(fields[0])
		if err != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", n, err))
			continue
		}
		for _, f := range fields[1:] {
			pattern, wildcard, err := parsePattern(f)
			if err != nil {
				errs = append(errs, fmt.Errorf("line %d: %w", n, err))
				continue
			}
			if prev, dup := seen[pattern]; dup {
				errs = append(errs, fmt.Errorf("line %d: duplicate host %q (already defined on line %d)", n, pattern, prev))
				continue
			}
			seen[pattern] = n
			rule := Rule{Pattern: pattern, Addr: addr, Port: port, Line: n, Text: line}
			if wildcard {
				s.wild[strings.TrimPrefix(pattern, "*.")] = rule
			} else {
				s.exact[pattern] = rule
			}
			s.all = append(s.all, rule)
		}
	}
	if err := sc.Err(); err != nil {
		errs = append(errs, fmt.Errorf("read rules: %w", err))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return s, nil
}

// parseTarget parses "ip", "ipv4:port" or "[ipv6]:port".
func parseTarget(s string) (netip.Addr, uint16, error) {
	if a, err := netip.ParseAddr(s); err == nil {
		if a.Zone() != "" {
			return netip.Addr{}, 0, fmt.Errorf("invalid address %q: zones are not supported", s)
		}
		return a.Unmap(), 0, nil
	}
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return netip.Addr{}, 0, fmt.Errorf("invalid address %q (want ip, ip:port or [ipv6]:port)", s)
	}
	if ap.Port() == 0 {
		return netip.Addr{}, 0, fmt.Errorf("invalid address %q: port must be in 1-65535", s)
	}
	if ap.Addr().Zone() != "" {
		return netip.Addr{}, 0, fmt.Errorf("invalid address %q: zones are not supported", s)
	}
	return ap.Addr().Unmap(), ap.Port(), nil
}

// parsePattern validates and normalizes a host or "*.domain" pattern.
func parsePattern(s string) (pattern string, wildcard bool, err error) {
	p := normalizeHost(s)
	host := p
	if strings.HasPrefix(p, "*.") {
		wildcard = true
		host = p[2:]
	}
	if !validHostname(host) {
		return "", false, fmt.Errorf("invalid host %q", s)
	}
	return p, wildcard, nil
}

// MatchPattern reports whether host matches pattern, using the same
// semantics as the rules file ("*.domain" matches subdomains only).
func MatchPattern(pattern, host string) bool {
	p, h := normalizeHost(pattern), normalizeHost(host)
	if p == "" || h == "" {
		return false
	}
	if suffix, ok := strings.CutPrefix(p, "*."); ok {
		return strings.HasSuffix(h, "."+suffix)
	}
	return p == h
}

func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ".")
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	return h
}

// validHostname accepts DNS names (letters, digits, '-', '_') and IP literals.
func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}
