package collector

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func ids(evs []Event) []uint64 {
	out := make([]uint64, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}

func eq(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRingBuffer(t *testing.T) {
	c := New(3)
	for i := 0; i < 5; i++ {
		c.Start(Event{Host: "h"})
	}
	if got := ids(c.List(Filter{})); !eq(got, []uint64{3, 4, 5}) {
		t.Fatalf("List = %v, want [3 4 5]", got)
	}
	if _, ok := c.Get(2); ok {
		t.Error("evicted event still found")
	}
	e, ok := c.Get(4)
	if !ok {
		t.Fatal("Get(4) not found")
	}
	e.Status = 204
	e.Done = true
	c.Update(e)
	if got, _ := c.Get(4); got.Status != 204 || !got.Done {
		t.Errorf("Update not applied: %+v", got)
	}
	// Updating an evicted event is harmless.
	c.Update(Event{ID: 1, Status: 500})
	if got := ids(c.List(Filter{})); !eq(got, []uint64{3, 4, 5}) {
		t.Errorf("List after stale update = %v", got)
	}
}

func TestListFilter(t *testing.T) {
	c := New(10)
	add := func(host string, total time.Duration) {
		e := c.Start(Event{Host: host})
		e.Total = total
		e.Done = true
		c.Update(e)
	}
	add("api.example.com", 10*time.Millisecond)
	add("other.net", 500*time.Millisecond)
	add("API.example.com", 900*time.Millisecond)
	add("x.example.com", 50*time.Millisecond)

	tests := []struct {
		name string
		f    Filter
		want []uint64
	}{
		{"all", Filter{}, []uint64{1, 2, 3, 4}},
		{"limit keeps most recent", Filter{Limit: 2}, []uint64{3, 4}},
		{"host substring case-insensitive", Filter{Host: "api.EXAMPLE"}, []uint64{1, 3}},
		{"min duration", Filter{MinTotal: 100 * time.Millisecond}, []uint64{2, 3}},
		{"combined", Filter{Host: "example", MinTotal: 40 * time.Millisecond, Limit: 1}, []uint64{4}},
	}
	for _, tt := range tests {
		if got := ids(c.List(tt.f)); !eq(got, tt.want) {
			t.Errorf("%s: List = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSubscribeAndClear(t *testing.T) {
	c := New(10)
	ch, cancel := c.Subscribe(10)
	e := c.Start(Event{Host: "a", ReqBody: []byte("secret body")})
	m := <-ch
	if m.Type != MsgEvent || m.Event.ID != e.ID {
		t.Fatalf("got %+v", m)
	}
	if m.Event.ReqBody != nil {
		t.Error("live update carries bodies")
	}
	if !m.Event.HasBodies() {
		t.Error("summary lost the has-bodies flag")
	}
	if err := c.Clear(); err != nil {
		t.Fatal(err)
	}
	if m := <-ch; m.Type != MsgClear {
		t.Errorf("got %v, want clear", m.Type)
	}
	if n := len(c.List(Filter{})); n != 0 {
		t.Errorf("%d events after clear", n)
	}
	// IDs keep increasing after a clear.
	if e2 := c.Start(Event{}); e2.ID != e.ID+1 {
		t.Errorf("ID after clear = %d", e2.ID)
	}
	cancel()
	cancel()
	if _, ok := <-ch; ok {
		// drain the event published before cancel
		if _, ok := <-ch; ok {
			t.Error("channel not closed")
		}
	}
	c.Start(Event{}) // must not panic on closed subscriber
}

func TestSlowSubscriberDoesNotBlock(t *testing.T) {
	c := New(10)
	_, cancel := c.Subscribe(1)
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			c.Start(Event{})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publishing blocked on a slow subscriber")
	}
}

func TestLoadWithGaps(t *testing.T) {
	c := New(5)
	c.Load([]Event{{ID: 3}, {ID: 7}, {ID: 8}})
	if _, ok := c.Get(7); !ok {
		t.Error("Get(7) not found")
	}
	if _, ok := c.Get(5); ok {
		t.Error("Get(5) found")
	}
	if e := c.Start(Event{}); e.ID != 9 {
		t.Errorf("next ID = %d, want 9", e.ID)
	}
}

func TestEventJSON(t *testing.T) {
	e := Event{
		ID: 7, Scheme: SchemeHTTP, Host: "a.com", Port: 80, Total: 1500 * time.Microsecond,
		TTFB: 2 * time.Millisecond, ReqHeader: http.Header{"Accept": {"*/*"}}, ReqBody: []byte("hi"),
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["total_ms"] != 1.5 || m["ttfb_ms"] != 2.0 || m["has_bodies"] != true {
		t.Errorf("unexpected JSON %s", b)
	}
	var back Event
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Total != e.Total || back.TTFB != e.TTFB || string(back.ReqBody) != "hi" || back.ReqHeader.Get("Accept") != "*/*" {
		t.Errorf("round trip = %+v", back)
	}
}
