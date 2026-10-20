// Package collector records proxied requests and tunnels, keeps the most
// recent ones in a ring buffer and fans them out to live subscribers.
package collector

import (
	"encoding/json"
	"net/http"
	"time"
)

// Scheme values.
const (
	SchemeHTTP        = "http"
	SchemeHTTPSTunnel = "https-tunnel"
	SchemeHTTPSMITM   = "https-mitm"
)

// Event describes one HTTP exchange (plain or MITM) or one CONNECT tunnel.
type Event struct {
	ID         uint64
	StartedAt  time.Time
	ClientAddr string
	Scheme     string // http | https-tunnel | https-mitm
	Host       string
	Port       uint16
	Method     string // empty for tunnels
	Path       string // empty for tunnels
	Status     int    // 0 for tunnels
	Route      string // "override:10.1.2.3" | "upstream:<addr>" | "direct"
	RuleMatch  string // applied rule line, if any

	// Durations, measured with the monotonic clock. Zero when unknown.
	// Phases are sequential: Blocked (connection pool wait, CONNECT
	// handshake with the upstream), DNS, Connect, TLS, TTFB; the transfer
	// time is what remains of Total.
	DNS, Connect, TLS, TTFB, Total time.Duration
	Blocked                        time.Duration

	BytesUp, BytesDown int64
	Err                string

	// Done is false while the exchange or tunnel is still in progress.
	Done bool
	// Reused is true when an idle keep-alive connection was reused, in
	// which case DNS, Connect and TLS are zero.
	Reused bool

	// Headers are only known for plain HTTP and MITM; sensitive values are
	// masked.
	ReqHeader  http.Header
	RespHeader http.Header

	// Bodies are only captured with --capture-bodies, up to a size limit.
	ReqBody, RespBody           []byte
	ReqTruncated, RespTruncated bool
}

// Summary returns a copy of e without bodies, for lists and live updates.
func (e Event) Summary() Event {
	e.ReqBody, e.RespBody = nil, nil
	return e
}

// HasBodies reports whether a request or response body was captured.
func (e Event) HasBodies() bool { return len(e.ReqBody) > 0 || len(e.RespBody) > 0 }

// jsonEvent is the wire format: durations in milliseconds.
type jsonEvent struct {
	ID            uint64      `json:"id"`
	StartedAt     time.Time   `json:"started_at"`
	ClientAddr    string      `json:"client_addr"`
	Scheme        string      `json:"scheme"`
	Host          string      `json:"host"`
	Port          uint16      `json:"port"`
	Method        string      `json:"method,omitempty"`
	Path          string      `json:"path,omitempty"`
	Status        int         `json:"status,omitempty"`
	Route         string      `json:"route"`
	RuleMatch     string      `json:"rule_match,omitempty"`
	BlockedMs     float64     `json:"blocked_ms"`
	DNSMs         float64     `json:"dns_ms"`
	ConnectMs     float64     `json:"connect_ms"`
	TLSMs         float64     `json:"tls_ms"`
	TTFBMs        float64     `json:"ttfb_ms"`
	TotalMs       float64     `json:"total_ms"`
	BytesUp       int64       `json:"bytes_up"`
	BytesDown     int64       `json:"bytes_down"`
	Err           string      `json:"err,omitempty"`
	Done          bool        `json:"done"`
	Reused        bool        `json:"reused,omitempty"`
	ReqHeader     http.Header `json:"req_header,omitempty"`
	RespHeader    http.Header `json:"resp_header,omitempty"`
	ReqBody       []byte      `json:"req_body,omitempty"` // base64
	RespBody      []byte      `json:"resp_body,omitempty"`
	ReqTruncated  bool        `json:"req_truncated,omitempty"`
	RespTruncated bool        `json:"resp_truncated,omitempty"`
	HasBodies     bool        `json:"has_bodies,omitempty"`
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func fromMs(f float64) time.Duration { return time.Duration(f * float64(time.Millisecond)) }

// MarshalJSON encodes durations as milliseconds.
func (e Event) MarshalJSON() ([]byte, error) {
	return json.Marshal(jsonEvent{
		ID: e.ID, StartedAt: e.StartedAt, ClientAddr: e.ClientAddr, Scheme: e.Scheme,
		Host: e.Host, Port: e.Port, Method: e.Method, Path: e.Path, Status: e.Status,
		Route: e.Route, RuleMatch: e.RuleMatch,
		BlockedMs: ms(e.Blocked), DNSMs: ms(e.DNS), ConnectMs: ms(e.Connect), TLSMs: ms(e.TLS), TTFBMs: ms(e.TTFB), TotalMs: ms(e.Total),
		BytesUp: e.BytesUp, BytesDown: e.BytesDown, Err: e.Err, Done: e.Done, Reused: e.Reused,
		ReqHeader: e.ReqHeader, RespHeader: e.RespHeader,
		ReqBody: e.ReqBody, RespBody: e.RespBody,
		ReqTruncated: e.ReqTruncated, RespTruncated: e.RespTruncated,
		HasBodies: e.HasBodies(),
	})
}

// UnmarshalJSON is the inverse of MarshalJSON.
func (e *Event) UnmarshalJSON(b []byte) error {
	var j jsonEvent
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*e = Event{
		ID: j.ID, StartedAt: j.StartedAt, ClientAddr: j.ClientAddr, Scheme: j.Scheme,
		Host: j.Host, Port: j.Port, Method: j.Method, Path: j.Path, Status: j.Status,
		Route: j.Route, RuleMatch: j.RuleMatch,
		Blocked: fromMs(j.BlockedMs), DNS: fromMs(j.DNSMs), Connect: fromMs(j.ConnectMs), TLS: fromMs(j.TLSMs), TTFB: fromMs(j.TTFBMs), Total: fromMs(j.TotalMs),
		BytesUp: j.BytesUp, BytesDown: j.BytesDown, Err: j.Err, Done: j.Done, Reused: j.Reused,
		ReqHeader: j.ReqHeader, RespHeader: j.RespHeader,
		ReqBody: j.ReqBody, RespBody: j.RespBody,
		ReqTruncated: j.ReqTruncated, RespTruncated: j.RespTruncated,
	}
	return nil
}
