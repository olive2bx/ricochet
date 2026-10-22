package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/olive2bx/ricochet/internal/dashboard"
)

// Exit codes of --ping and --stop.
const (
	ctlOK         = 0
	ctlNotRunning = 1
	ctlError      = 2
)

// instance is what GET /api/status reports.
type instance struct {
	App      string `json:"app"`
	PID      int    `json:"pid"`
	Version  string `json:"version"`
	Proxy    string `json:"proxy"`
	Upstream string `json:"upstream"`
	MITM     string `json:"mitm"` // "all", a host list, or "" when off
	CA       string `json:"ca"`   // absolute path of the MITM CA certificate
}

var errNotRunning = errors.New("no ricochet dashboard answering")

// ctlClient talks to the local dashboard directly, whatever the proxy
// environment variables say (they may point at ricochet itself).
func ctlClient() *http.Client {
	return &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
}

// queryStatus returns the instance whose dashboard listens on addr.
func queryStatus(c *http.Client, addr string) (instance, error) {
	resp, err := c.Get("http://" + addr + "/api/status")
	if err != nil {
		return instance{}, errNotRunning
	}
	defer resp.Body.Close()
	var in instance
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&in) != nil || in.App != "ricochet" {
		return instance{}, errNotRunning
	}
	return in, nil
}

// cmdPing prints the running instance as key=value lines. Nothing is
// written when no instance answers, so scripts can rely on the exit code.
func cmdPing(addr string, out io.Writer) int {
	in, err := queryStatus(ctlClient(), addr)
	if err != nil {
		return ctlNotRunning
	}
	fmt.Fprintf(out, "pid=%d\nversion=%s\nproxy=%s\nupstream=%s\nmitm=%s\nca=%s\n",
		in.PID, in.Version, in.Proxy, in.Upstream, in.MITM, in.CA)
	return ctlOK
}

// cmdStop asks the instance on addr to shut down gracefully and waits until
// its dashboard stops answering.
func cmdStop(addr string, out io.Writer, wait time.Duration) int {
	c := ctlClient()
	in, err := queryStatus(c, addr)
	if err != nil {
		return ctlNotRunning
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/shutdown", nil)
	if err != nil {
		fmt.Fprintf(out, "error=%v\n", err)
		return ctlError
	}
	req.Header.Set(dashboard.ControlHeader, "1")
	resp, err := c.Do(req)
	if err != nil {
		fmt.Fprintf(out, "error=%v\n", err)
		return ctlError
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		fmt.Fprintf(out, "error=shutdown refused: %s\n", resp.Status)
		return ctlError
	}
	// Done once neither the dashboard nor the proxy port answer, so that a
	// new instance can bind them right away.
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if _, err := queryStatus(c, addr); err == nil {
			continue
		}
		// Binding the proxy port tells whether it is free without opening a
		// connection, which would delay the graceful shutdown.
		if in.Proxy != "" {
			ln, err := net.Listen("tcp", in.Proxy)
			if err != nil {
				continue
			}
			ln.Close()
		}
		fmt.Fprintf(out, "pid=%d\n", in.PID)
		return ctlOK
	}
	fmt.Fprintf(out, "pid=%d\nerror=still running after %s\n", in.PID, wait)
	return ctlError
}
