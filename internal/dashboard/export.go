package dashboard

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/olive2bx/ricochet/internal/collector"
)

// HAR 1.2 subset (http://www.softwareishard.com/blog/har-12-spec/).
type harLog struct {
	Log harBody `json:"log"`
}

type harBody struct {
	Version string     `json:"version"`
	Creator harCreator `json:"creator"`
	Entries []harEntry `json:"entries"`
}

type harCreator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type harEntry struct {
	StartedDateTime string      `json:"startedDateTime"`
	Time            float64     `json:"time"`
	Request         harRequest  `json:"request"`
	Response        harResponse `json:"response"`
	Cache           struct{}    `json:"cache"`
	Timings         harTimings  `json:"timings"`
	ServerIPAddress string      `json:"serverIPAddress,omitempty"`
	Comment         string      `json:"comment,omitempty"`
	// Custom fields (allowed by the spec with a leading underscore).
	Scheme string `json:"_scheme"`
	Route  string `json:"_route"`
	Rule   string `json:"_rule,omitempty"`
	Error  string `json:"_error,omitempty"`
	Client string `json:"_clientAddr,omitempty"`
}

type harNV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type harRequest struct {
	Method      string       `json:"method"`
	URL         string       `json:"url"`
	HTTPVersion string       `json:"httpVersion"`
	Headers     []harNV      `json:"headers"`
	QueryString []harNV      `json:"queryString"`
	Cookies     []harNV      `json:"cookies"`
	HeadersSize int          `json:"headersSize"`
	BodySize    int64        `json:"bodySize"`
	PostData    *harPostData `json:"postData,omitempty"`
}

type harPostData struct {
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
	Comment  string `json:"comment,omitempty"`
}

type harResponse struct {
	Status      int        `json:"status"`
	StatusText  string     `json:"statusText"`
	HTTPVersion string     `json:"httpVersion"`
	Headers     []harNV    `json:"headers"`
	Cookies     []harNV    `json:"cookies"`
	Content     harContent `json:"content"`
	RedirectURL string     `json:"redirectURL"`
	HeadersSize int        `json:"headersSize"`
	BodySize    int64      `json:"bodySize"`
}

type harContent struct {
	Size     int64  `json:"size"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text,omitempty"`
	Encoding string `json:"encoding,omitempty"`
	Comment  string `json:"comment,omitempty"`
}

type harTimings struct {
	Blocked float64 `json:"blocked"`
	DNS     float64 `json:"dns"`
	Connect float64 `json:"connect"` // includes ssl, per the spec
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
	SSL     float64 `json:"ssl"`
}

func msf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func harHeaders(h http.Header) []harNV {
	out := []harNV{}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range h[k] {
			out = append(out, harNV{Name: k, Value: v})
		}
	}
	return out
}

func eventURL(e collector.Event) string {
	scheme := "http"
	if e.Scheme != collector.SchemeHTTP {
		scheme = "https"
	}
	host := e.Host
	if (scheme == "http" && e.Port != 80) || (scheme == "https" && e.Port != 443) {
		host += ":" + strconv.Itoa(int(e.Port))
	}
	if e.Scheme == collector.SchemeHTTPSTunnel {
		return scheme + "://" + host + "/"
	}
	return scheme + "://" + host + e.Path
}

// bodyText returns the HAR text and encoding of a captured body.
func bodyText(b []byte) (string, string) {
	if utf8.Valid(b) {
		return string(b), ""
	}
	return base64.StdEncoding.EncodeToString(b), "base64"
}

func toHAR(events []collector.Event, version string) harLog {
	entries := make([]harEntry, 0, len(events))
	for _, e := range events {
		u := eventURL(e)
		method := e.Method
		if e.Scheme == collector.SchemeHTTPSTunnel {
			method = http.MethodConnect
		}
		var query []harNV
		if pu, err := url.Parse(u); err == nil {
			for k, vs := range pu.Query() {
				for _, v := range vs {
					query = append(query, harNV{Name: k, Value: v})
				}
			}
		}
		if query == nil {
			query = []harNV{}
		}
		known := e.Blocked + e.DNS + e.Connect + e.TLS + e.TTFB
		entry := harEntry{
			StartedDateTime: e.StartedAt.Format(time.RFC3339Nano),
			Time:            msf(e.Total),
			Request: harRequest{
				Method: method, URL: u, HTTPVersion: "HTTP/1.1",
				Headers: harHeaders(e.ReqHeader), QueryString: query, Cookies: []harNV{},
				HeadersSize: -1, BodySize: e.BytesUp,
			},
			Response: harResponse{
				Status: e.Status, StatusText: http.StatusText(e.Status), HTTPVersion: "HTTP/1.1",
				Headers: harHeaders(e.RespHeader), Cookies: []harNV{},
				Content:     harContent{Size: e.BytesDown, MimeType: e.RespHeader.Get("Content-Type")},
				HeadersSize: -1, BodySize: e.BytesDown,
			},
			Timings: harTimings{
				Blocked: msf(e.Blocked),
				DNS:     msf(e.DNS),
				Connect: msf(e.Connect + e.TLS),
				Wait:    msf(e.TTFB),
				Receive: msf(max(0, e.Total-known)),
				SSL:     msf(e.TLS),
			},
			Scheme: e.Scheme, Route: e.Route, Rule: e.RuleMatch, Error: e.Err, Client: e.ClientAddr,
		}
		if e.Reused {
			entry.Timings.DNS, entry.Timings.Connect, entry.Timings.SSL = -1, -1, -1
		}
		if len(e.ReqBody) > 0 {
			text, enc := bodyText(e.ReqBody)
			pd := &harPostData{MimeType: e.ReqHeader.Get("Content-Type"), Text: text}
			if enc != "" {
				pd.Comment = "base64-encoded"
			}
			if e.ReqTruncated {
				pd.Comment = strings.TrimPrefix(pd.Comment+", truncated", ", ")
			}
			entry.Request.PostData = pd
		}
		if len(e.RespBody) > 0 {
			entry.Response.Content.Text, entry.Response.Content.Encoding = bodyText(e.RespBody)
			if e.RespTruncated {
				entry.Response.Content.Comment = "truncated"
			}
		}
		if e.Scheme == collector.SchemeHTTPSTunnel {
			entry.Comment = "encrypted tunnel: only connection-level timings and byte counts are known"
		}
		entries = append(entries, entry)
	}
	return harLog{Log: harBody{Version: "1.2", Creator: harCreator{Name: "ricochet", Version: version}, Entries: entries}}
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	events := s.opt.Collector.All()
	stamp := time.Now().Format("20060102-150405")
	var v any
	switch format := r.URL.Query().Get("format"); format {
	case "", "har":
		v = toHAR(events, s.opt.Version)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="ricochet-%s.har"`, stamp))
	case "json":
		v = events
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="ricochet-%s.json"`, stamp))
	default:
		http.Error(w, "unknown format (har or json)", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		s.log.Debug("export", "err", err)
	}
}
