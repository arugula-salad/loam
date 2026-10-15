package nodelet

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/pkg/api"
)

// maxBody bounds a request body; the largest is a CreateVM with its wake's env.
const maxBody = 1 << 20

// Handler serves the node protocol (docs/NODE.md). Every request must carry
// the node token.
func (n *Nodelet) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/node", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, n.Node())
	})
	mux.HandleFunc("GET /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, nodeapi.VMList{VMs: n.VMs()})
	})
	mux.HandleFunc("POST /v1/vms", func(w http.ResponseWriter, r *http.Request) {
		var req nodeapi.CreateVM
		if !decode(w, r, &req) {
			return
		}
		v, err := n.Create(r.Context(), req)
		respond(w, http.StatusCreated, v, err)
	})
	mux.HandleFunc("GET /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := n.VM(r.PathValue("id"))
		respond(w, http.StatusOK, v, err)
	})
	mux.HandleFunc("DELETE /v1/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := n.Delete(r.PathValue("id")); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/vms/{id}/resume", func(w http.ResponseWriter, r *http.Request) {
		var req nodeapi.Resume
		if !decode(w, r, &req) {
			return
		}
		v, err := n.Resume(r.Context(), r.PathValue("id"), req)
		respond(w, http.StatusOK, v, err)
	})
	mux.HandleFunc("PUT /v1/vms/{id}/policy", func(w http.ResponseWriter, r *http.Request) {
		var req nodeapi.Policy
		if !decode(w, r, &req) {
			return
		}
		if err := n.SetPolicy(r.Context(), r.PathValue("id"), req); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/vms/{id}/pause", func(w http.ResponseWriter, r *http.Request) {
		var req nodeapi.PauseVM
		if !decode(w, r, &req) {
			return
		}
		v, err := n.Pause(r.Context(), r.PathValue("id"), req.Snapshot)
		respond(w, http.StatusOK, v, err)
	})
	mux.HandleFunc("POST /v1/vms/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		var req nodeapi.StopVM
		if !decode(w, r, &req) {
			return
		}
		v, err := n.Stop(r.Context(), r.PathValue("id"), req.Snapshot)
		respond(w, http.StatusOK, v, err)
	})
	mux.HandleFunc("POST /v1/vms/{id}/snapshots", func(w http.ResponseWriter, r *http.Request) {
		var req nodeapi.CreateSnapshot
		if !decode(w, r, &req) {
			return
		}
		v, err := n.Snapshot(r.Context(), r.PathValue("id"), req)
		respond(w, http.StatusCreated, v, err)
	})
	mux.HandleFunc("POST /v1/vms/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		var req nodeapi.Restore
		if !decode(w, r, &req) {
			return
		}
		v, err := n.Restore(r.Context(), r.PathValue("id"), req)
		respond(w, http.StatusOK, v, err)
	})
	mux.HandleFunc("GET /v1/vms/{id}/activity", func(w http.ResponseWriter, r *http.Request) {
		v, err := n.Activity(r.Context(), r.PathValue("id"))
		respond(w, http.StatusOK, v, err)
	})
	mux.HandleFunc("GET /v1/vms/{id}/agent", n.serveAgent)
	mux.HandleFunc("GET /v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, nodeapi.SnapshotList{Snapshots: n.Snapshots()})
	})
	mux.HandleFunc("GET /v1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		s, err := n.GetSnapshot(r.Context(), r.PathValue("id"))
		respond(w, http.StatusOK, s, err)
	})
	mux.HandleFunc("DELETE /v1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := n.DeleteSnapshot(r.Context(), r.PathValue("id")); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /v1/layers/{layer}", func(w http.ResponseWriter, r *http.Request) {
		if err := n.DeleteLayer(r.Context(), r.PathValue("layer")); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/events", n.serveEvents)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, errorf(http.StatusNotFound, api.CodeNotFound, "no route %s %s", r.Method, r.URL.Path))
	})
	return n.authorize(mux)
}

func (n *Nodelet) authorize(h http.Handler) http.Handler {
	want := []byte("Bearer " + n.opts.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if n.opts.Token == "" || subtle.ConstantTimeCompare(got, want) != 1 {
			writeError(w, errorf(http.StatusUnauthorized, api.CodeUnauthorized, "a node token is required"))
			return
		}
		h.ServeHTTP(w, r)
	})
}

// decode reads a JSON body into v, refusing fields it doesn't know: a field
// the nodelet would ignore is an option it can't honour.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, invalid("bad request body: %v", err))
		return false
	}
	return true
}

// respond writes a result or its error.
func respond(w http.ResponseWriter, status int, v any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, status, v)
}

// serveAgent is the agent stream: checked, upgraded, and spliced to the
// agent's vsock port. The stream to the agent is opened before the upgrade,
// so a VM that isn't running, or an agent that doesn't answer, is an error
// answer rather than a stream that closes at once.
func (n *Nodelet) serveAgent(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != nodeapi.UpgradeAgent {
		writeError(w, invalid("the agent stream needs Upgrade: %s", nodeapi.UpgradeAgent))
		return
	}
	agent, err := n.DialAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	c, err := nodeapi.AcceptUpgrade(w, r)
	if err != nil {
		agent.Close()
		writeError(w, fmt.Errorf("upgrade: %w", err))
		return
	}
	nodeapi.Splice(c, agent)
}
