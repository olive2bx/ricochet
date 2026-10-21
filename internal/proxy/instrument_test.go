package proxy

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/olive2bx/ricochet/internal/collector"
)

// resetConn is a server connection that delivers some bytes, then a reset.
type resetConn struct {
	net.Conn
	sent bool
}

var errReset = errors.New("read: connection reset by peer")

func (c *resetConn) Read(b []byte) (int, error) {
	if !c.sent {
		c.sent = true
		return copy(b, "hello"), nil
	}
	return 0, errReset
}
func (c *resetConn) Write(b []byte) (int, error) { return len(b), nil }
func (c *resetConn) Close() error                { return nil }
func (c *resetConn) CloseWrite() error           { return nil }

func TestTunnelResetAfterClientDone(t *testing.T) {
	tests := []struct {
		name       string
		clientDone bool
		wantErr    string
	}{
		{"reset during exchange is an error", false, errReset.Error()},
		{"reset after client half-close is teardown noise", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			col := collector.New(4)
			p := &Proxy{col: col, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			ev := col.Start(collector.Event{Scheme: collector.SchemeHTTPSTunnel})
			tc := newTunnelConn(p, &resetConn{}, time.Now(), ev)

			tc.Write([]byte("request"))
			b := make([]byte, 16)
			if _, err := tc.Read(b); err != nil {
				t.Fatal(err)
			}
			if tt.clientDone {
				tc.CloseWrite()
			}
			if _, err := tc.Read(b); !errors.Is(err, errReset) {
				t.Fatalf("read err = %v", err)
			}
			tc.Close()

			got, _ := col.Get(ev.ID)
			if !got.Done || got.Err != tt.wantErr || got.BytesUp != 7 || got.BytesDown != 5 {
				t.Errorf("event = done:%v err:%q up:%d down:%d", got.Done, got.Err, got.BytesUp, got.BytesDown)
			}
		})
	}
}
