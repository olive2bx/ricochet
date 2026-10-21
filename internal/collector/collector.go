package collector

import (
	"strings"
	"sync"
	"time"
)

// MessageType distinguishes live updates.
type MessageType string

const (
	MsgEvent MessageType = "event" // an event was added or updated
	MsgClear MessageType = "clear" // the history was cleared
)

// Message is a live update sent to subscribers. Event carries no bodies.
type Message struct {
	Type  MessageType
	Event Event
}

// Collector stores events in a fixed-size ring buffer. It is safe for
// concurrent use.
type Collector struct {
	mu     sync.Mutex
	nextID uint64
	buf    []Event // ring, events have consecutive IDs
	head   int     // index of the oldest event
	n      int     // number of stored events
	subs   map[chan Message]struct{}
	sink   Sink
}

// New returns a collector keeping the last capacity events.
func New(capacity int) *Collector {
	if capacity < 1 {
		capacity = 1
	}
	return &Collector{
		nextID: 1,
		buf:    make([]Event, capacity),
		subs:   map[chan Message]struct{}{},
	}
}

// Start records a new event, assigns its ID (and StartedAt if zero) and
// returns it.
func (c *Collector) Start(e Event) Event {
	if e.StartedAt.IsZero() {
		e.StartedAt = time.Now()
	}
	c.mu.Lock()
	e.ID = c.nextID
	c.nextID++
	c.push(e)
	c.publish(Message{Type: MsgEvent, Event: e.Summary()})
	c.mu.Unlock()
	c.persist(e)
	return e
}

// Update replaces the stored event with the same ID. Events already evicted
// from the ring are still published and persisted.
func (c *Collector) Update(e Event) {
	c.mu.Lock()
	if i, ok := c.index(e.ID); ok {
		c.buf[i] = e
	}
	c.publish(Message{Type: MsgEvent, Event: e.Summary()})
	c.mu.Unlock()
	c.persist(e)
}

func (c *Collector) persist(e Event) {
	if c.sink != nil && e.Done {
		c.sink.Save(e)
	}
}

// Get returns the event with the given ID, bodies included.
func (c *Collector) Get(id uint64) (Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i, ok := c.index(id); ok {
		return c.buf[i], true
	}
	return Event{}, false
}

// Filter selects events in List.
type Filter struct {
	Limit    int           // max number of events, the most recent ones; 0 = all
	Host     string        // case-insensitive substring of Host
	MinTotal time.Duration // minimum Total duration
}

func (f Filter) match(e *Event) bool {
	if f.Host != "" && !strings.Contains(strings.ToLower(e.Host), strings.ToLower(f.Host)) {
		return false
	}
	return e.Total >= f.MinTotal
}

// List returns matching events, oldest first, without bodies.
func (c *Collector) List(f Filter) []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Event
	for k := c.n - 1; k >= 0; k-- {
		e := &c.buf[(c.head+k)%len(c.buf)]
		if !f.match(e) {
			continue
		}
		out = append(out, e.Summary())
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// All returns every stored event, oldest first, bodies included.
func (c *Collector) All() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Event, 0, c.n)
	for k := 0; k < c.n; k++ {
		out = append(out, c.buf[(c.head+k)%len(c.buf)])
	}
	return out
}

// Clear empties the history (and the sink, if any).
func (c *Collector) Clear() error {
	c.mu.Lock()
	clear(c.buf)
	c.head, c.n = 0, 0
	c.publish(Message{Type: MsgClear})
	c.mu.Unlock()
	if c.sink != nil {
		return c.sink.Clear()
	}
	return nil
}

// Subscribe returns a channel of live updates and a function to stop them.
// Updates are dropped for a subscriber whose buffer is full.
func (c *Collector) Subscribe(buffer int) (<-chan Message, func()) {
	ch := make(chan Message, buffer)
	c.mu.Lock()
	c.subs[ch] = struct{}{}
	c.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			c.mu.Lock()
			delete(c.subs, ch)
			c.mu.Unlock()
			close(ch)
		})
	}
}

// publish must be called with c.mu held.
func (c *Collector) publish(m Message) {
	for ch := range c.subs {
		select {
		case ch <- m:
		default:
		}
	}
}

// push must be called with c.mu held.
func (c *Collector) push(e Event) {
	if c.n < len(c.buf) {
		c.buf[(c.head+c.n)%len(c.buf)] = e
		c.n++
		return
	}
	c.buf[c.head] = e
	c.head = (c.head + 1) % len(c.buf)
}

// index returns the ring index of id. IDs increase along the ring but may
// have gaps (history loaded from a sink). Must be called with c.mu held.
func (c *Collector) index(id uint64) (int, bool) {
	lo, hi := 0, c.n
	for lo < hi {
		mid := (lo + hi) / 2
		if c.buf[(c.head+mid)%len(c.buf)].ID < id {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == c.n {
		return 0, false
	}
	i := (c.head + lo) % len(c.buf)
	return i, c.buf[i].ID == id
}

// Sink persists finished events.
type Sink interface {
	Save(Event)
	Clear() error
}

// SetSink sets the sink receiving finished events. Call before use.
func (c *Collector) SetSink(s Sink) { c.sink = s }

// Load preloads events (oldest first, increasing IDs), e.g. from a sink.
// Call before use.
func (c *Collector) Load(events []Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range events {
		c.push(e)
		if e.ID >= c.nextID {
			c.nextID = e.ID + 1
		}
	}
}
