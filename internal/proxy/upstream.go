package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// connectTimeout bounds the CONNECT handshake with the upstream proxy.
const connectTimeout = 30 * time.Second

// connectThrough performs a CONNECT handshake for target on conn, an open
// connection to an HTTP proxy. On success the returned conn is a raw tunnel
// to target. conn is closed on error.
func connectThrough(ctx context.Context, conn net.Conn, target, auth string) (_ net.Conn, err error) {
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()

	deadline := time.Now().Add(connectTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("upstream CONNECT %s: %w", target, err)
	}
	// Abort the handshake if ctx is cancelled.
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: target},
		Host:   target,
		Header: http.Header{},
	}
	if auth != "" {
		req.Header.Set("Proxy-Authorization", basicAuth(auth))
	}
	if err := req.Write(conn); err != nil {
		return nil, fmt.Errorf("upstream CONNECT %s: write: %w", target, err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, fmt.Errorf("upstream CONNECT %s: read response: %w", target, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream CONNECT %s: %s", target, resp.Status)
	}

	if !stop() {
		return nil, fmt.Errorf("upstream CONNECT %s: %w", target, ctx.Err())
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("upstream CONNECT %s: %w", target, err)
	}
	if n := br.Buffered(); n > 0 {
		// The upstream already sent tunnel bytes; keep them.
		peek, _ := br.Peek(n)
		return &prefixConn{Conn: conn, prefix: append([]byte(nil), peek...)}, nil
	}
	return conn, nil
}

func basicAuth(userpass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(userpass))
}

// prefixConn replays bytes read ahead before reading from Conn.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

// CloseWrite and CloseRead let goproxy half-close the tunnel when the
// underlying conn supports it.
func (c *prefixConn) CloseWrite() error {
	if hc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return c.Conn.Close()
}

func (c *prefixConn) CloseRead() error {
	if hc, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return hc.CloseRead()
	}
	return nil
}
