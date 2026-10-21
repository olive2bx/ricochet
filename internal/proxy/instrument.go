package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/elazarl/goproxy"

	"github.com/olive2bx/ricochet/internal/collector"
)

// DefaultMaskedHeaders are the headers whose values are hidden by default.
var DefaultMaskedHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie"}

const maskedValue = "[masked]"

// maskHeader returns a copy of h with the values of the masked headers
// replaced.
func (p *Proxy) maskHeader(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := h.Clone()
	for _, name := range p.mask {
		if vs, ok := out[name]; ok {
			for i := range vs {
				vs[i] = maskedValue
			}
		}
	}
	return out
}

func (p *Proxy) record(e collector.Event) collector.Event {
	if p.col == nil {
		return e
	}
	return p.col.Start(e)
}

func (p *Proxy) update(e collector.Event) {
	if p.col != nil {
		p.col.Update(e)
	}
}

// phaseTimer collects httptrace timestamps. Callbacks may run on other
// goroutines (e.g. an abandoned dial), hence the mutex.
type phaseTimer struct {
	mu                  sync.Mutex
	start               time.Time
	dnsStart, dnsDone   time.Time
	connStart, connDone time.Time
	tlsStart, tlsDone   time.Time
	gotConn, firstByte  time.Time
	reused              bool
}

func (t *phaseTimer) trace() *httptrace.ClientTrace {
	set := func(dst *time.Time) {
		t.mu.Lock()
		if dst.IsZero() {
			*dst = time.Now()
		}
		t.mu.Unlock()
	}
	return &httptrace.ClientTrace{
		DNSStart:          func(httptrace.DNSStartInfo) { set(&t.dnsStart) },
		DNSDone:           func(httptrace.DNSDoneInfo) { set(&t.dnsDone) },
		ConnectStart:      func(string, string) { set(&t.connStart) },
		ConnectDone:       func(string, string, error) { set(&t.connDone) },
		TLSHandshakeStart: func() { set(&t.tlsStart) },
		TLSHandshakeDone:  func(tls.ConnectionState, error) { set(&t.tlsDone) },
		GotConn: func(info httptrace.GotConnInfo) {
			t.mu.Lock()
			if t.gotConn.IsZero() {
				t.gotConn = time.Now()
				t.reused = info.Reused
			}
			t.mu.Unlock()
		},
		GotFirstResponseByte: func() { set(&t.firstByte) },
	}
}

func span(a, b time.Time) time.Duration {
	if a.IsZero() || b.IsZero() || b.Before(a) {
		return 0
	}
	return b.Sub(a)
}

// apply fills the phase durations of e. Phases are sequential:
// Blocked (pool wait, upstream CONNECT), DNS, Connect, TLS, TTFB (request
// sent → first response byte), then transfer = Total - all of them.
func (t *phaseTimer) apply(e *collector.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e.Reused = t.reused
	if !t.reused {
		e.DNS = span(t.dnsStart, t.dnsDone)
		e.Connect = span(t.connStart, t.connDone)
		e.TLS = span(t.tlsStart, t.tlsDone)
	}
	if !t.gotConn.IsZero() {
		e.Blocked = max(0, span(t.start, t.gotConn)-e.DNS-e.Connect-e.TLS)
		e.TTFB = span(t.gotConn, t.firstByte)
	}
}

// installRoundTripper makes every plain HTTP and MITM request go through
// the instrumented round tripper.
func (p *Proxy) installRoundTripper() {
	rt := goproxy.RoundTripperFunc(p.roundTrip)
	p.gp.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		ctx.RoundTripper = rt
		return req, nil
	})
}

func (p *Proxy) roundTrip(req *http.Request, _ *goproxy.ProxyCtx) (*http.Response, error) {
	start := time.Now()
	scheme := collector.SchemeHTTP
	if req.URL.Scheme == "https" {
		scheme = collector.SchemeHTTPSMITM
	}
	hostport := hostPortDefault(req.URL.Host, defaultPort(req.URL.Scheme))
	host, port := splitPort(hostport)
	route, _ := p.router.Route(hostport)

	ev := p.record(collector.Event{
		StartedAt:  start,
		ClientAddr: req.RemoteAddr,
		Scheme:     scheme,
		Host:       host,
		Port:       port,
		Method:     req.Method,
		Path:       req.URL.RequestURI(),
		Route:      route.String(),
		RuleMatch:  ruleText(route),
		ReqHeader:  p.maskHeader(req.Header),
	})

	timer := &phaseTimer{start: start}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), timer.trace()))

	var reqBody *countingReader
	if req.Body != nil && req.Body != http.NoBody {
		reqBody = &countingReader{rc: req.Body, capture: p.captureLimit}
		req.Body = reqBody
	}

	resp, err := p.tr.RoundTrip(req)
	timer.apply(&ev)
	if reqBody != nil {
		ev.BytesUp = reqBody.n.Load()
	}
	if err != nil {
		ev.Err = err.Error()
		ev.Total = time.Since(start)
		ev.Done = true
		if reqBody != nil {
			ev.ReqBody, ev.ReqTruncated = reqBody.captured()
		}
		p.update(ev)
		p.log.Warn("request failed", "host", hostport, "route", ev.Route, "err", err)
		return nil, err
	}

	ev.Status = resp.StatusCode
	ev.RespHeader = p.maskHeader(resp.Header)
	p.update(ev)

	resp.Body = &countingReader{
		rc:      resp.Body,
		capture: p.captureLimit,
		onDone: func(down int64, captured []byte, truncated bool, rerr error) {
			ev.BytesDown = down
			ev.RespBody, ev.RespTruncated = captured, truncated
			if reqBody != nil {
				ev.BytesUp = reqBody.n.Load()
				ev.ReqBody, ev.ReqTruncated = reqBody.captured()
			}
			if rerr != nil && ev.Err == "" {
				ev.Err = rerr.Error()
			}
			ev.Total = time.Since(start)
			ev.Done = true
			p.update(ev)
			p.log.Debug("request done", "method", ev.Method, "host", hostport, "path", ev.Path,
				"status", ev.Status, "route", ev.Route, "total", ev.Total)
		},
	}
	return resp, nil
}

func splitPort(hostport string) (string, uint16) {
	host, ps, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, 0
	}
	port, _ := strconv.ParseUint(ps, 10, 16)
	return host, uint16(port)
}

// countingReader counts (and optionally captures) the bytes of a body and
// reports once on EOF, error or Close.
type countingReader struct {
	rc      io.ReadCloser
	n       atomic.Int64
	capture int // max bytes to capture, 0 = none

	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool

	once   sync.Once
	onDone func(n int64, captured []byte, truncated bool, err error)
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.rc.Read(b)
	c.n.Add(int64(n))
	if c.capture > 0 && n > 0 {
		c.mu.Lock()
		room := c.capture - c.buf.Len()
		if room < n {
			c.truncated = true
		}
		if room > 0 {
			c.buf.Write(b[:min(n, room)])
		}
		c.mu.Unlock()
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			c.done(nil)
		} else {
			c.done(err)
		}
	}
	return n, err
}

func (c *countingReader) Close() error {
	err := c.rc.Close()
	c.done(nil)
	return err
}

func (c *countingReader) captured() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.buf.Len() == 0 {
		return nil, c.truncated
	}
	return bytes.Clone(c.buf.Bytes()), c.truncated
}

func (c *countingReader) done(err error) {
	if c.onDone == nil {
		return
	}
	c.once.Do(func() {
		b, tr := c.captured()
		c.onDone(c.n.Load(), b, tr, err)
	})
}

// tunnelConn wraps the server side of a CONNECT tunnel to measure it.
// Writes go to the server (bytes up), reads come from it (bytes down).
type tunnelConn struct {
	net.Conn
	p     *Proxy
	start time.Time

	up, down            atomic.Int64
	firstUp, firstDown  atomic.Int64 // ns since start, 0 = not yet
	errMu               sync.Mutex
	err                 error
	once                sync.Once
	ev                  collector.Event
	closeWrite, closeRd func() error
	// clientDone is set once the client finished sending (half-close). Errors
	// after that, typically a reset from a server closing with unread data,
	// are teardown noise rather than failures of the exchange.
	clientDone atomic.Bool
}

func newTunnelConn(p *Proxy, c net.Conn, start time.Time, ev collector.Event) *tunnelConn {
	t := &tunnelConn{Conn: c, p: p, start: start, ev: ev}
	if hc, ok := c.(interface{ CloseWrite() error }); ok {
		t.closeWrite = hc.CloseWrite
	}
	if hc, ok := c.(interface{ CloseRead() error }); ok {
		t.closeRd = hc.CloseRead
	}
	return t
}

func (t *tunnelConn) mark(first *atomic.Int64) {
	if first.Load() == 0 {
		first.CompareAndSwap(0, max(1, int64(time.Since(t.start))))
	}
}

func (t *tunnelConn) setErr(err error) {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || t.clientDone.Load() {
		return
	}
	// An encrypted tunnel cannot tell a finished exchange from a cut one.
	// Servers commonly reset or abort the connection when closing it, so a
	// reset once data flowed both ways counts as a normal close.
	if isConnReset(err) && t.up.Load() > 0 && t.down.Load() > 0 {
		return
	}
	t.errMu.Lock()
	if t.err == nil {
		t.err = err
	}
	t.errMu.Unlock()
}

func (t *tunnelConn) Write(b []byte) (int, error) {
	if len(b) > 0 {
		t.mark(&t.firstUp)
	}
	n, err := t.Conn.Write(b)
	t.up.Add(int64(n))
	t.setErr(err)
	return n, err
}

func (t *tunnelConn) Read(b []byte) (int, error) {
	n, err := t.Conn.Read(b)
	if n > 0 {
		t.mark(&t.firstDown)
	}
	t.down.Add(int64(n))
	t.setErr(err)
	return n, err
}

// CloseWrite and CloseRead keep goproxy's half-close path available.
func (t *tunnelConn) CloseWrite() error {
	t.clientDone.Store(true)
	if t.closeWrite != nil {
		return t.closeWrite()
	}
	return t.Close()
}

func (t *tunnelConn) CloseRead() error {
	if t.closeRd != nil {
		return t.closeRd()
	}
	return nil
}

func (t *tunnelConn) Close() error {
	err := t.Conn.Close()
	t.once.Do(t.finish)
	return err
}

func (t *tunnelConn) finish() {
	e := t.ev
	e.Total = time.Since(t.start)
	e.BytesUp, e.BytesDown = t.up.Load(), t.down.Load()
	// TTFB: first server byte after the first client byte.
	fu, fd := time.Duration(t.firstUp.Load()), time.Duration(t.firstDown.Load())
	switch {
	case fd > 0 && fu > 0 && fd > fu:
		e.TTFB = fd - fu
	case fd > 0 && fu == 0:
		e.TTFB = fd - e.DNS - e.Connect - e.Blocked
	}
	t.errMu.Lock()
	if t.err != nil {
		e.Err = t.err.Error()
	}
	t.errMu.Unlock()
	e.Done = true
	t.p.update(e)
	t.p.log.Debug("tunnel closed", "host", e.Host, "route", e.Route, "up", e.BytesUp, "down", e.BytesDown, "total", e.Total)
}

// isConnReset reports a connection reset or abort. Windows reports them as
// WSAECONNABORTED (10053) and WSAECONNRESET (10054), which the syscall
// package does not map to ECONNABORTED/ECONNRESET.
func isConnReset(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.ECONNRESET || errno == syscall.ECONNABORTED || errno == 10053 || errno == 10054
}

// traceDial dials addr while measuring DNS and TCP connect.
func (p *Proxy) traceDial(ctx context.Context, network, addr string, e *collector.Event) (net.Conn, error) {
	timer := &phaseTimer{}
	conn, err := p.dialer.DialContext(httptrace.WithClientTrace(ctx, timer.trace()), network, addr)
	timer.mu.Lock()
	e.DNS = span(timer.dnsStart, timer.dnsDone)
	e.Connect = span(timer.connStart, timer.connDone)
	timer.mu.Unlock()
	return conn, err
}
