package nodelet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// event is one on GET /v1/events: kind is nodeapi.EventVM (data a VM),
// nodeapi.EventSnapshot (data a stored Snapshot) or nodeapi.EventNode (data
// the Node, not ready, as a drain starts).
type event struct {
	kind string
	data any
}

// bus fans VM state changes that no request made, and stored snapshots
// (docs/NODE.md, "Events"), out to the SSE subscribers. A subscriber that falls behind loses events rather
// than holding up the VM that changed: loam-api reconciles from GET /v1/vms on
// every reconnect anyway, and a lagging one is disconnected so it does.
type bus struct {
	mu   sync.Mutex
	subs map[chan event]struct{}
}

const subscriberBuffer = 64

func newBus() *bus { return &bus{subs: map[chan event]struct{}{}} }

func (b *bus) subscribe() (<-chan event, func()) {
	ch := make(chan event, subscriberBuffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
}

func (b *bus) publish(v event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- v:
		default:
			delete(b.subs, ch)
			close(ch)
		}
	}
}

// delivered waits, up to timeout, until every subscriber has taken every
// event published so far, and a moment for its handler to write and flush
// it: a drain with nothing to pause would otherwise exit before loam-api
// hears that the node is going.
func (b *bus) delivered(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		pending := 0
		for ch := range b.subs {
			pending += len(ch)
		}
		b.mu.Unlock()
		if pending == 0 {
			time.Sleep(100 * time.Millisecond)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sseKeepalive is how often an idle event stream gets a comment line, so
// proxies and loam-api notice a dead connection.
const sseKeepalive = 15 * time.Second

func (n *Nodelet) serveEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, cancel := n.events.subscribe()
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	tick := time.NewTicker(sseKeepalive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			fmt.Fprint(w, ": keepalive\n\n")
		case e, ok := <-ch:
			if !ok {
				return // fell behind; the client reconnects and reconciles
			}
			b, _ := json.Marshal(e.data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.kind, b)
		}
		fl.Flush()
	}
}
