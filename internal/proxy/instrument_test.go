package proxy

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/olive2bx/ricochet/internal/collector"
)

// resetConn is a server connection that delivers reply (if any), then
// fails reads with err.
type resetConn struct {
	net.Conn
	reply string
	err   error
}

// Resets as returned by the net package on Unix and on Windows.
var (
	errReset    = &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
	errAbortWin = &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("wsarecv", syscall.Errno(10053))}
	errTimeout  = &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
)

func (c *resetConn) Read(b []byte) (int, error) {
	if c.reply != "" {
		n := copy(b, c.reply)
		c.reply = ""
		return n, nil
	}
	return 0, c.err
}
func (c *resetConn) Write(b []byte) (int, error) { return len(b), nil }
func (c *resetConn) Close() error                { return nil }
func (c *resetConn) CloseWrite() error           { return nil }

func TestTunnelTeardownErrors(t *testing.T) {
	tests := []struct {
		name       string
		reply      string // bytes the server sends before failing
		err        error
		clientDone bool
		wantErr    bool
	}{
		{"reset before any server byte is an error", "", errReset, false, true},
		{"reset after data both ways is a close", "hello", errReset, false, false},
		{"windows abort after data both ways is a close", "hello", errAbortWin, false, false},
		{"timeout after data is an error", "hello", errTimeout, false, true},
		{"any error after client half-close is a close", "", errTimeout, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			col := collector.New(4)
			p := &Proxy{col: col, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			ev := col.Start(collector.Event{Scheme: collector.SchemeHTTPSTunnel})
			tc := newTunnelConn(p, &resetConn{reply: tt.reply, err: tt.err}, time.Now(), ev)

			tc.Write([]byte("request"))
			b := make([]byte, 16)
			if tt.reply != "" {
				if _, err := tc.Read(b); err != nil {
					t.Fatal(err)
				}
			}
			if tt.clientDone {
				tc.CloseWrite()
			}
			if _, err := tc.Read(b); !errors.Is(err, tt.err) {
				t.Fatalf("read err = %v", err)
			}
			tc.Close()

			got, _ := col.Get(ev.ID)
			if !got.Done || (got.Err != "") != tt.wantErr || got.BytesUp != 7 || got.BytesDown != int64(len(tt.reply)) {
				t.Errorf("event = done:%v err:%q up:%d down:%d", got.Done, got.Err, got.BytesUp, got.BytesDown)
			}
		})
	}
}
