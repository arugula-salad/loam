package db

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
)

// Memory is the Store in memory: the handler tests' store, and loam-api's when
// it runs without a database (local development). Nothing survives a restart.
type Memory struct {
	mu               sync.Mutex
	projects         map[string]api.Project
	keys             map[string]Key
	sandboxes        map[string]Sandbox
	checkpoints      map[string][]api.Checkpoint // by sandbox, oldest first
	ports            map[string]map[int]string
	events           []Event
	snapshots        map[string]Snapshot
	snapSeq          int64
	layers           map[string]bool
	tombstones       map[[2]string]bool
	secrets          map[[2]string]Secret // by project and name
	e2b              map[string]E2BSandbox
	e2bTemplates     map[string]E2BTemplate
	e2bTemplateFiles map[string]map[string][]byte
	traffic          map[[2]string]*memTraffic // by sandbox and day
	salts            map[string][]byte
	e2bSecrets       map[string]E2BSecret
}

// memTraffic is a day's counts and its set of visitor hashes.
type memTraffic struct {
	api.Traffic
	visitors map[string]bool
}

func NewMemory() *Memory {
	return &Memory{
		projects:    map[string]api.Project{},
		keys:        map[string]Key{},
		sandboxes:   map[string]Sandbox{},
		checkpoints: map[string][]api.Checkpoint{},
		ports:       map[string]map[int]string{},
		snapshots:   map[string]Snapshot{},
		layers:      map[string]bool{},
		tombstones:  map[[2]string]bool{},
		secrets:     map[[2]string]Secret{},
		e2b:         map[string]E2BSandbox{},
		traffic:     map[[2]string]*memTraffic{},
		salts:       map[string][]byte{},
	}
}

func (m *Memory) Close() {}

func (m *Memory) CreateProject(_ context.Context, p api.Project) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[p.Name]; ok {
		return ErrExists
	}
	m.projects[p.Name] = p
	return nil
}

func (m *Memory) Project(_ context.Context, name string) (api.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.projects[name]
	if !ok {
		return p, ErrNotFound
	}
	return p, nil
}

func (m *Memory) Projects(context.Context) ([]api.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]api.Project, 0, len(m.projects))
	for _, p := range m.projects {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (m *Memory) DeleteProject(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[name]; !ok {
		return ErrNotFound
	}
	for _, s := range m.sandboxes {
		if s.Project == name {
			return ErrNotEmpty
		}
	}
	for id, t := range m.e2bTemplates {
		if t.Project == name {
			delete(m.e2bTemplates, id)
			delete(m.e2bTemplateFiles, id)
		}
	}
	delete(m.projects, name)
	for id, k := range m.keys {
		if k.Project == name {
			delete(m.keys, id)
		}
	}
	for id, s := range m.e2bSecrets {
		if s.Project == name {
			delete(m.e2bSecrets, id)
		}
	}
	for k := range m.secrets {
		if k[0] == name {
			delete(m.secrets, k)
		}
	}
	return nil
}

func (m *Memory) CreateKey(_ context.Context, k Key) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[k.Project]; !ok {
		return ErrNotFound
	}
	for _, o := range m.keys {
		if o.ID == k.ID || bytes.Equal(o.Hash, k.Hash) || (o.Project == k.Project && o.Name == k.Name) {
			return ErrExists
		}
	}
	m.keys[k.ID] = k
	return nil
}

func (m *Memory) Keys(_ context.Context, project string) ([]Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Key
	for _, k := range m.keys {
		if k.Project == project {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *Memory) KeyByHash(_ context.Context, hash []byte) (Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range m.keys {
		if bytes.Equal(k.Hash, hash) {
			return k, nil
		}
	}
	return Key{}, ErrNotFound
}

func (m *Memory) DeleteKey(_ context.Context, project, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, k := range m.keys {
		if k.Project == project && (k.ID == key || k.Name == key) {
			delete(m.keys, id)
			n++
		}
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (m *Memory) CreateSandbox(_ context.Context, s Sandbox) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[s.Project]; !ok {
		return ErrNotFound
	}
	if _, ok := m.sandboxes[s.ID]; ok {
		return ErrExists
	}
	for _, o := range m.sandboxes {
		if o.Project == s.Project && o.Name == s.Name {
			return ErrExists
		}
	}
	if err := m.checkSecrets(s); err != nil {
		return err
	}
	m.sandboxes[s.ID] = s.Clone()
	return nil
}

// checkSecrets is ErrNoSecret when s names a secret its project doesn't have.
func (m *Memory) checkSecrets(s Sandbox) error {
	for _, name := range s.Secrets {
		if _, ok := m.secrets[[2]string{s.Project, name}]; !ok {
			return fmt.Errorf("%w: %q", ErrNoSecret, name)
		}
	}
	return nil
}

func (m *Memory) Sandbox(_ context.Context, id string) (Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sandboxes[id]
	if !ok {
		return Sandbox{}, ErrNotFound
	}
	return s.Clone(), nil
}

func (m *Memory) SandboxByName(_ context.Context, project, name string) (Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sandboxes {
		if s.Project == project && s.Name == name {
			return s.Clone(), nil
		}
	}
	return Sandbox{}, ErrNotFound
}

func (m *Memory) Sandboxes(_ context.Context, project string) ([]Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Sandbox
	for _, s := range m.sandboxes {
		if project == "" || s.Project == project {
			out = append(out, s.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (m *Memory) UpdateSandbox(_ context.Context, id string, change func(*Sandbox) error) (Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sandboxes[id]
	if !ok {
		return Sandbox{}, ErrNotFound
	}
	s = s.Clone()
	if err := change(&s); err != nil {
		return Sandbox{}, err
	}
	s.ID = id
	if err := m.checkSecrets(s); err != nil {
		return Sandbox{}, err
	}
	m.sandboxes[id] = s.Clone()
	return s, nil
}

func (m *Memory) DeleteSandbox(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sandboxes[id]; !ok {
		return ErrNotFound
	}
	delete(m.sandboxes, id)
	delete(m.checkpoints, id)
	delete(m.ports, id)
	delete(m.e2b, id)
	for k := range m.traffic {
		if k[0] == id {
			delete(m.traffic, k)
		}
	}
	return nil
}

func (m *Memory) CreateCheckpoint(_ context.Context, c api.Checkpoint) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sandboxes[c.Sandbox]; !ok {
		return ErrNotFound
	}
	for _, o := range m.checkpoints[c.Sandbox] {
		if o.ID == c.ID {
			return ErrExists
		}
	}
	m.checkpoints[c.Sandbox] = append(m.checkpoints[c.Sandbox], c)
	return nil
}

func (m *Memory) Checkpoint(_ context.Context, sandbox, id string) (api.Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.checkpoints[sandbox] {
		if c.ID == id {
			return c, nil
		}
	}
	return api.Checkpoint{}, ErrNotFound
}

func (m *Memory) Checkpoints(_ context.Context, sandbox string) ([]api.Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]api.Checkpoint(nil), m.checkpoints[sandbox]...), nil
}

func (m *Memory) DeleteCheckpoint(_ context.Context, sandbox, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs := m.checkpoints[sandbox]
	for i, c := range cs {
		if c.ID == id {
			m.checkpoints[sandbox] = append(cs[:i:i], cs[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) SetPort(_ context.Context, p PortSetting) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sandboxes[p.Sandbox]; !ok {
		return ErrNotFound
	}
	if m.ports[p.Sandbox] == nil {
		m.ports[p.Sandbox] = map[int]string{}
	}
	m.ports[p.Sandbox][p.Port] = p.Access
	return nil
}

func (m *Memory) Ports(_ context.Context, sandbox string) ([]PortSetting, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []PortSetting
	for port, access := range m.ports[sandbox] {
		out = append(out, PortSetting{Sandbox: sandbox, Port: port, Access: access})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out, nil
}

func (m *Memory) AddEvent(_ context.Context, e Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
	return nil
}

func (m *Memory) Events(_ context.Context, project string, since time.Time) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for _, e := range m.events {
		if e.Project == project && !e.At.Before(since) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *Memory) PruneEvents(_ context.Context, before time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.events[:0]
	for _, e := range m.events {
		if !e.At.Before(before) {
			kept = append(kept, e)
		}
	}
	m.events = kept
	return nil
}

func (m *Memory) AddTraffic(_ context.Context, ts []Traffic) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range ts {
		if _, ok := m.sandboxes[t.Sandbox]; !ok {
			continue
		}
		k := [2]string{t.Sandbox, t.Day}
		mt := m.traffic[k]
		if mt == nil {
			mt = &memTraffic{visitors: map[string]bool{}}
			m.traffic[k] = mt
		}
		mt.Requests += t.Requests
		mt.Status2xx += t.Status2xx
		mt.Status3xx += t.Status3xx
		mt.Status4xx += t.Status4xx
		mt.Status5xx += t.Status5xx
		mt.WebSockets += t.WebSockets
		mt.BytesSent += t.BytesSent
		mt.KeyRequests += t.KeyRequests
		for _, v := range t.NewVisitors {
			mt.visitors[string(v)] = true
		}
	}
	return nil
}

func (m *Memory) TrafficOf(_ context.Context, sandbox, since string) ([]Traffic, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Traffic
	for k, mt := range m.traffic {
		// Days are 2006-01-02, so they compare as strings.
		if k[0] == sandbox && k[1] >= since {
			out = append(out, Traffic{Sandbox: sandbox, Day: k[1], Traffic: mt.Traffic, Visitors: int64(len(mt.visitors))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out, nil
}

func (m *Memory) PruneTraffic(_ context.Context, before string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.traffic {
		if k[1] < before {
			delete(m.traffic, k)
		}
	}
	return nil
}

func (m *Memory) VisitorSalt(_ context.Context, day string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.salts[day]; ok {
		return slices.Clone(s), nil
	}
	s := make([]byte, 32)
	if _, err := rand.Read(s); err != nil {
		return nil, err
	}
	for d := range m.salts {
		if d < day {
			delete(m.salts, d)
		}
	}
	m.salts[day] = s
	return slices.Clone(s), nil
}

func (m *Memory) addLayers(ls []string) {
	for _, l := range ls {
		m.layers[l] = true
	}
}

func (m *Memory) CreateSnapshot(_ context.Context, s Snapshot) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.snapshots[s.ID]; ok {
		return s, ErrExists
	}
	m.snapSeq++
	s = s.Clone()
	s.Seq = m.snapSeq
	m.snapshots[s.ID] = s
	m.addLayers(s.Layers)
	return s.Clone(), nil
}

func (m *Memory) Snapshot(_ context.Context, id string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.snapshots[id]
	if !ok {
		return Snapshot{}, ErrNotFound
	}
	return s.Clone(), nil
}

func (m *Memory) snapshotsWhere(keep func(Snapshot) bool) []Snapshot {
	var out []Snapshot
	for _, s := range m.snapshots {
		if keep(s) {
			out = append(out, s.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

func (m *Memory) SnapshotsOf(_ context.Context, sandbox string) ([]Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotsWhere(func(s Snapshot) bool { return s.Sandbox == sandbox }), nil
}

func (m *Memory) SnapshotsOn(_ context.Context, node string) ([]Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotsWhere(func(s Snapshot) bool { return s.Node == node }), nil
}

func (m *Memory) UpdateSnapshot(_ context.Context, id string, change func(*Snapshot) error) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.snapshots[id]
	if !ok {
		return Snapshot{}, ErrNotFound
	}
	s = s.Clone()
	seq := s.Seq
	if err := change(&s); err != nil {
		return Snapshot{}, err
	}
	s.ID, s.Seq = id, seq
	m.snapshots[id] = s.Clone()
	m.addLayers(s.Layers)
	return s, nil
}

func (m *Memory) DeleteSnapshot(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.snapshots[id]; !ok {
		return ErrNotFound
	}
	delete(m.snapshots, id)
	return nil
}

// GarbageSnapshots is the rule in db.go, the same as Postgres's query.
func (m *Memory) GarbageSnapshots(context.Context) ([]Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	named := map[string]bool{}
	for _, sb := range m.sandboxes {
		if sb.Snapshot != "" {
			named[sb.Snapshot] = true
		}
	}
	return m.snapshotsWhere(func(s Snapshot) bool {
		if s.State == SnapPending || named[s.ID] {
			return false
		}
		switch {
		case s.State == SnapAbandoned:
			return true
		case s.Kind == SnapCheckpoint:
			for _, c := range m.checkpoints[s.Sandbox] {
				if c.ID == s.Checkpoint {
					return false
				}
			}
			return true
		}
		if _, ok := m.sandboxes[s.Sandbox]; !ok {
			return true
		}
		for _, n := range m.snapshots {
			if n.Sandbox == s.Sandbox && n.Stored && n.State == SnapReady && n.Seq > s.Seq {
				return true
			}
		}
		return false
	}), nil
}

func (m *Memory) Layers(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for l := range m.layers {
		out = append(out, l)
	}
	sort.Strings(out)
	return out, nil
}

func (m *Memory) referenced(layer string) bool {
	for _, s := range m.snapshots {
		for _, l := range s.Layers {
			if l == layer {
				return true
			}
		}
	}
	return false
}

func (m *Memory) UnreferencedLayers(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for l := range m.layers {
		if !m.referenced(l) {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *Memory) DeleteLayer(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.layers[id] {
		return ErrNotFound
	}
	if m.referenced(id) {
		return ErrInUse
	}
	delete(m.layers, id)
	return nil
}

func (m *Memory) AddLayers(_ context.Context, ids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addLayers(ids)
	return nil
}

func (m *Memory) AddTombstone(_ context.Context, node, vm string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tombstones[[2]string{node, vm}] = true
	return nil
}

func (m *Memory) Tombstones(_ context.Context, node string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.tombstones {
		if k[0] == node {
			out = append(out, k[1])
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *Memory) DeleteTombstone(_ context.Context, node, vm string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := [2]string{node, vm}
	if !m.tombstones[k] {
		return ErrNotFound
	}
	delete(m.tombstones, k)
	return nil
}

func (m *Memory) PutSecret(_ context.Context, s Secret) (Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[s.Project]; !ok {
		return Secret{}, ErrNotFound
	}
	k := [2]string{s.Project, s.Name}
	if old, ok := m.secrets[k]; ok {
		s.CreatedAt = old.CreatedAt
	}
	m.secrets[k] = s.Clone()
	return s.Clone(), nil
}

func (m *Memory) Secret(_ context.Context, project, name string) (Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.secrets[[2]string{project, name}]
	if !ok {
		return Secret{}, ErrNotFound
	}
	return s.Clone(), nil
}

func (m *Memory) Secrets(_ context.Context, project string) ([]Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Secret
	for k, s := range m.secrets {
		if k[0] == project {
			out = append(out, s.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Memory) DeleteSecret(_ context.Context, project, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := [2]string{project, name}
	if _, ok := m.secrets[k]; !ok {
		return ErrNotFound
	}
	for _, sb := range m.sandboxes {
		if sb.Project == project && slices.Contains(sb.Secrets, name) {
			return ErrInUse
		}
	}
	delete(m.secrets, k)
	return nil
}
