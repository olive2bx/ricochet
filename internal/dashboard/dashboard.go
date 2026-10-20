// Package dashboard serves the web UI, the JSON API and the live SSE feed.
package dashboard

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/olive2bx/ricochet/internal/collector"
	"github.com/olive2bx/ricochet/internal/rules"
)

//go:embed web
var webFS embed.FS

// Options configures the dashboard.
type Options struct {
	Collector *collector.Collector
	// Rules returns the current rules status. May be nil.
	Rules func() rules.Status
	// Info is shown in the UI header (listen address, upstream, MITM...).
	Info   map[string]string
	Logger *slog.Logger
	// AllowedHosts are extra Host header values accepted besides loopback
	// names, e.g. the dashboard listen host when not on loopback.
	AllowedHosts []string
}

// Server is the dashboard HTTP handler.
type Server struct {
	opt Options
	log *slog.Logger
	mux *http.ServeMux
}

// New builds the dashboard handler.
func New(opt Options) *Server {
	s := &Server{opt: opt, log: opt.Logger, mux: http.NewServeMux()}
	if s.log == nil {
		s.log = slog.Default()
	}
	web, _ := fs.Sub(webFS, "web")
	s.mux.Handle("GET /", http.FileServerFS(web))
	s.mux.HandleFunc("GET /api/requests", s.handleList)
	s.mux.HandleFunc("GET /api/requests/{id}", s.handleGet)
	s.mux.HandleFunc("GET /api/rules", s.handleRules)
	s.mux.HandleFunc("GET /api/info", s.handleInfo)
	s.mux.HandleFunc("POST /api/clear", s.handleClear)
	s.mux.HandleFunc("GET /events", s.handleEvents)
	return s
}

// ServeHTTP implements http.Handler. Requests whose Host is not a loopback
// name (DNS rebinding) or cross-origin writes are rejected.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.hostAllowed(r.Host) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if o := r.Header.Get("Origin"); o != "" && !sameOrigin(o, r.Host) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; img-src 'self' data:")
	s.mux.ServeHTTP(w, r)
}

// Handle registers an extra handler (e.g. exports).
func (s *Server) Handle(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

func (s *Server) hostAllowed(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if a, err := netip.ParseAddr(host); err == nil && a.IsLoopback() {
		return true
	}
	for _, h := range s.opt.AllowedHosts {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

func sameOrigin(origin, host string) bool {
	o := strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://")
	return strings.EqualFold(o, host)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	if err := enc.Encode(v); err != nil {
		slog.Debug("write json", "err", err)
	}
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := collector.Filter{Host: q.Get("host")}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		f.Limit = n
	}
	if v := q.Get("min_ms"); v != "" {
		ms, err := strconv.ParseFloat(v, 64)
		if err != nil || ms < 0 {
			http.Error(w, "invalid min_ms", http.StatusBadRequest)
			return
		}
		f.MinTotal = time.Duration(ms * float64(time.Millisecond))
	}
	events := s.opt.Collector.List(f)
	if events == nil {
		events = []collector.Event{}
	}
	writeJSON(w, events)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	e, ok := s.opt.Collector.Get(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, e)
}

type rulesJSON struct {
	Path     string     `json:"path"`
	Rules    []ruleJSON `json:"rules"`
	LoadedAt *time.Time `json:"loaded_at,omitempty"`
	LastErr  string     `json:"last_error,omitempty"`
}

type ruleJSON struct {
	Pattern string `json:"pattern"`
	IP      string `json:"ip"`
	Port    uint16 `json:"port,omitempty"`
	Line    int    `json:"line"`
	Text    string `json:"text"`
}

func (s *Server) handleRules(w http.ResponseWriter, _ *http.Request) {
	out := rulesJSON{Rules: []ruleJSON{}}
	if s.opt.Rules != nil {
		st := s.opt.Rules()
		out.Path = st.Path
		if !st.LoadedAt.IsZero() {
			out.LoadedAt = &st.LoadedAt
		}
		if st.LastErr != nil {
			out.LastErr = st.LastErr.Error()
		}
		for _, r := range st.Rules {
			out.Rules = append(out.Rules, ruleJSON{Pattern: r.Pattern, IP: r.Addr.String(), Port: r.Port, Line: r.Line, Text: r.Text})
		}
	}
	writeJSON(w, out)
}

func (s *Server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	info := s.opt.Info
	if info == nil {
		info = map[string]string{}
	}
	writeJSON(w, info)
}

func (s *Server) handleClear(w http.ResponseWriter, _ *http.Request) {
	if err := s.opt.Collector.Clear(); err != nil {
		s.log.Warn("clear history", "err", err)
		http.Error(w, "clear: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleEvents streams live updates as Server-Sent Events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, cancel := s.opt.Collector.Subscribe(1024)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "retry: 2000\n: connected\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
		case m, ok := <-ch:
			if !ok {
				return
			}
			switch m.Type {
			case collector.MsgClear:
				fmt.Fprint(w, "event: clear\ndata: {}\n\n")
			default:
				b, err := json.Marshal(m.Event)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "event: event\ndata: %s\n\n", b)
			}
		}
		flusher.Flush()
	}
}
