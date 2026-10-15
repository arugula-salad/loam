package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/arugula-salad/loam/internal/db"
	"github.com/arugula-salad/loam/pkg/api"
)

// Events are best effort (docs/API.md, "Events"): the API is the state, and a
// client that reconnects reads it. Each one is written to the events table and
// published on an in-memory bus to the SSE streams open in this process
// (single replica: see server.go).

type subscriber struct {
	project string
	sandbox string // "" is every sandbox of the project
	ch      chan api.Event
}

type bus struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

func newBus() *bus { return &bus{subs: map[*subscriber]struct{}{}} }

func (b *bus) subscribe(project, sandbox string) (*subscriber, func()) {
	s := &subscriber{project: project, sandbox: sandbox, ch: make(chan api.Event, 256)}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s, func() {
		b.mu.Lock()
		delete(b.subs, s)
		b.mu.Unlock()
	}
}

// publish never blocks: a subscriber too slow to keep 256 events loses some,
// which best effort allows.
func (b *bus) publish(project string, e api.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		if s.project == project && (s.sandbox == "" || s.sandbox == e.Sandbox) {
			select {
			case s.ch <- e:
			default:
			}
		}
	}
}

func (s *Server) emit(sb db.Sandbox, typ string, detail map[string]any) {
	e := api.Event{Type: typ, Sandbox: sb.ID, At: s.now(), Detail: detail}
	if err := s.store.AddEvent(context.Background(), db.Event{Project: sb.Project, Event: e}); err != nil {
		s.log.Warn("event not stored", "type", typ, "sandbox", sb.ID, "err", err)
	}
	s.bus.publish(sb.Project, e)
}

// events is GET /v1/events?sandbox=: Server-Sent Events for the key's project.
func (s *Server) events(w http.ResponseWriter, r *http.Request, c caller) {
	filter := ""
	if ref := r.URL.Query().Get("sandbox"); ref != "" {
		sb, err := s.lookup(r.Context(), c, ref)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		filter = sb.ID
	}
	sub, done := s.bus.subscribe(c.project.Name, filter)
	defer done()
	fl, ok := w.(http.Flusher)
	if !ok {
		s.writeErr(w, errf(http.StatusInternalServerError, "internal", "streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
		case e := <-sub.ch:
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, b)
		}
		fl.Flush()
	}
}
