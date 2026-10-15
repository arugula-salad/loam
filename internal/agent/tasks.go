package agent

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
)

// A task is an explicit keep-awake: while one is live the sandbox is busy
// (/internal/activity) and loam-api doesn't put it to sleep for being idle.
// Every task expires (an hour at most), so a client that crashed cannot pin
// a VM forever; holding longer means refreshing it.

const (
	maxTaskExpire = time.Hour
	maxTasks      = 64
	maxTaskName   = 128
)

// taskTable's zero value is ready to use. Times are wall-clock (no monotonic
// reading), so a task's expiry counts the time the sandbox spent paused.
type taskTable struct {
	mu sync.Mutex
	m  map[string]api.Task
}

// live drops expired tasks and returns the rest sorted by name.
func (t *taskTable) live() []api.Task {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now().Round(0)
	out := []api.Task{}
	for name, task := range t.m {
		if !now.Before(task.ExpiresAt) {
			delete(t.m, name)
			continue
		}
		out = append(out, task)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (t *taskTable) get(name string) (api.Task, bool) {
	for _, task := range t.live() {
		if task.Name == name {
			return task, true
		}
	}
	return api.Task{}, false
}

var errTaskLimit = fmt.Errorf("too many tasks (at most %d)", maxTasks)

// put creates a task or refreshes an existing one's expiry.
func (t *taskTable) put(name string, expire time.Duration) (api.Task, error) {
	live := t.live()
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now().UTC().Round(0)
	task, exists := t.m[name]
	if !exists {
		if len(live) >= maxTasks {
			return task, errTaskLimit
		}
		task = api.Task{Name: name, StartedAt: now}
	}
	task.ExpiresAt = now.Add(expire)
	if t.m == nil {
		t.m = map[string]api.Task{}
	}
	t.m[name] = task
	return task, nil
}

func (t *taskTable) delete(name string) bool {
	_, ok := t.get(name)
	t.mu.Lock()
	delete(t.m, name)
	t.mu.Unlock()
	return ok
}

func (s *Server) registerTasks(mux *http.ServeMux) {
	mux.HandleFunc("GET /tasks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, api.TaskList{Tasks: s.tasks.live()})
	})
	mux.HandleFunc("GET /tasks/{name}", func(w http.ResponseWriter, r *http.Request) {
		if task, ok := s.tasks.get(r.PathValue("name")); ok {
			writeJSON(w, http.StatusOK, task)
			return
		}
		writeErr(w, http.StatusNotFound, api.CodeNotFound, "no task "+r.PathValue("name"))
	})
	mux.HandleFunc("PUT /tasks/{name}", s.taskPut)
	mux.HandleFunc("DELETE /tasks/{name}", func(w http.ResponseWriter, r *http.Request) {
		if !s.tasks.delete(r.PathValue("name")) {
			writeErr(w, http.StatusNotFound, api.CodeNotFound, "no task "+r.PathValue("name"))
			return
		}
		// The idle window starts when the hold ends, not when it began.
		s.touch()
		w.WriteHeader(http.StatusNoContent)
	})
}

func (s *Server) taskPut(w http.ResponseWriter, r *http.Request) {
	var req api.PutTask
	if !decodeBody(w, r, 1<<16, &req) {
		return
	}
	name := r.PathValue("name")
	if name == "" || len(name) > maxTaskName {
		writeErr(w, http.StatusBadRequest, api.CodeInvalid, fmt.Sprintf("a task name is 1-%d bytes", maxTaskName))
		return
	}
	expire := maxTaskExpire
	if req.ExpireS != 0 {
		expire = time.Duration(req.ExpireS) * time.Second
	}
	if expire <= 0 || expire > maxTaskExpire {
		writeErr(w, http.StatusBadRequest, api.CodeInvalid, "expire_s must be between 1 and 3600; refresh the task to hold longer")
		return
	}
	task, err := s.tasks.put(name, expire)
	if errors.Is(err, errTaskLimit) {
		writeErr(w, http.StatusConflict, api.CodeConflict, err.Error())
		return
	}
	s.touch()
	writeJSON(w, http.StatusOK, task)
}
