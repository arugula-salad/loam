package apiserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/arugula-salad/loam/internal/nodeapi"
)

// Nodes finds the nodelets. A record stores a node's name, never its address
// (docs/NODE.md, "Reaching a nodelet"): Client resolves the name each time,
// through a cache of a few seconds at most, because a nodelet pod that
// restarts comes back with a new IP, and a remembered one would send
// requests to whatever pod has it now.
type Nodes struct {
	Token string
	// Service is the nodelets' headless Service. Resolving it gives one address
	// per nodelet pod; asking each GET /v1/node gives its name.
	Service string
	// Static names nodelets by hand (local development): name → host:port.
	// With Static set, Service is not used.
	Static map[string]string
	// Resolve looks Service up; nil is the system resolver.
	Resolve func(ctx context.Context, host string) ([]string, error)
	// Port is the nodelets' port behind Service; 0 is nodeapi.Port.
	Port int
	HTTP *http.Client
	// Wait is how long ClientWait waits for a node it can't reach; 0 is nodeWait.
	Wait time.Duration
	// Now is the clock seen times are kept by; nil is time.Now. Server.New
	// sets it to the server's.
	Now func() time.Time

	mu     sync.Mutex
	cache  map[string]string // name → host:port
	cached time.Time
	seen   map[string]seenNode
}

// seenNode is when a node last answered GET /v1/node, and whether it was ready.
type seenNode struct {
	at    time.Time
	ready bool
}

// cacheFor is how long a resolved name→address map is trusted.
const cacheFor = 3 * time.Second

// ErrNoNode is a node name no nodelet answers to now.
var ErrNoNode = errors.New("no nodelet answers to that node name")

// NodeInfo is one nodelet as it answered GET /v1/node.
type NodeInfo struct {
	Addr string
	nodeapi.Node
}

func (n *Nodes) client(addr string) *nodeapi.Client {
	hc := n.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	return &nodeapi.Client{Addr: addr, Token: n.Token, HTTP: hc}
}

// addrs is every nodelet address to ask.
func (n *Nodes) addrs(ctx context.Context) ([]string, error) {
	if n.Static != nil {
		var out []string
		for _, a := range n.Static {
			out = append(out, a)
		}
		sort.Strings(out)
		return out, nil
	}
	resolve := n.Resolve
	if resolve == nil {
		resolve = net.DefaultResolver.LookupHost
	}
	hosts, err := resolve(ctx, n.Service)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", n.Service, err)
	}
	var out []string
	port := n.Port
	if port == 0 {
		port = nodeapi.Port
	}
	for _, h := range hosts {
		out = append(out, net.JoinHostPort(h, strconv.Itoa(port)))
	}
	sort.Strings(out)
	return out, nil
}

// List asks every nodelet for itself, in parallel. A nodelet that doesn't
// answer is left out: it can't take a sandbox now.
func (n *Nodes) List(ctx context.Context) ([]NodeInfo, error) {
	addrs, err := n.addrs(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	infos := make([]*NodeInfo, len(addrs))
	var wg sync.WaitGroup
	for i, a := range addrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if nd, err := n.client(a).Node(ctx); err == nil && nd.Name != "" {
				infos[i] = &NodeInfo{Addr: a, Node: nd}
			}
		}()
	}
	wg.Wait()
	var out []NodeInfo
	cache := map[string]string{}
	for _, in := range infos {
		if in != nil {
			out = append(out, *in)
			cache[in.Name] = in.Addr
		}
	}
	now := time.Now()
	if n.Now != nil {
		now = n.Now()
	}
	n.mu.Lock()
	n.cache, n.cached = cache, time.Now()
	if n.seen == nil {
		n.seen = map[string]seenNode{}
	}
	for _, in := range out {
		n.seen[in.Name] = seenNode{at: now, ready: in.Ready}
	}
	n.mu.Unlock()
	return out, nil
}

// markSeen records that node answered just now, ready or not: a nodelet
// announces its drain on its event stream (nodeapi.EventNode), which loam-api
// may otherwise never see between two calls if the drain is quick.
func (n *Nodes) markSeen(name string, ready bool) {
	now := time.Now()
	if n.Now != nil {
		now = n.Now()
	}
	n.mu.Lock()
	if n.seen == nil {
		n.seen = map[string]seenNode{}
	}
	n.seen[name] = seenNode{at: now, ready: ready}
	n.mu.Unlock()
}

// lastSeen is when node last answered and whether it was ready then; ok is
// false for a node this process has never heard from.
func (n *Nodes) lastSeen(name string) (seenNode, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	sn, ok := n.seen[name]
	return sn, ok
}

// Client is a client for the nodelet of node name, resolved now (or within
// the last few seconds).
func (n *Nodes) Client(ctx context.Context, name string) (*nodeapi.Client, error) {
	n.mu.Lock()
	addr, ok := n.cache[name]
	fresh := time.Since(n.cached) < cacheFor
	n.mu.Unlock()
	if !ok || !fresh {
		if _, err := n.List(ctx); err != nil {
			return nil, err
		}
		n.mu.Lock()
		addr, ok = n.cache[name]
		n.mu.Unlock()
	}
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNoNode, name)
	}
	return n.client(addr), nil
}

// nodeWait is how long a call waits for a node it can't resolve or reach:
// a nodelet that is restarting (a roll, a drain) is gone from the headless
// Service for a few seconds, and a request in that gap should wait for it
// rather than fail.
const nodeWait = 30 * time.Second

// ClientWait is Client, retried while the node can't be resolved, for up to
// nodeWait or until ctx ends.
func (n *Nodes) ClientWait(ctx context.Context, name string) (*nodeapi.Client, error) {
	wait := n.Wait
	if wait == 0 {
		wait = nodeWait
	}
	deadline := time.Now().Add(wait)
	for {
		cl, err := n.Client(ctx, name)
		if err == nil || time.Now().After(deadline) {
			return cl, err
		}
		n.mu.Lock()
		n.cached = time.Time{} // look again, not at the cache
		n.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// place picks the node for a sandbox of memMiB: the ready node with the most
// free memory that fits it, among nodes. Admission reserves the ceiling
// (docs/NODE.md, "The node"). None is "", which is capacity.
func place(nodes []NodeInfo, memMiB int) string {
	best, bestFree := "", -1
	for _, nd := range nodes {
		if nd.fits(memMiB) && nd.free() > bestFree {
			best, bestFree = nd.Name, nd.free()
		}
	}
	return best
}

func (nd NodeInfo) free() int { return nd.MemoryMiB - nd.MemoryMiBAllocated }

// fits says whether the node is ready and has room for memMiB more.
func (nd NodeInfo) fits(memMiB int) bool { return nd.Ready && nd.free() >= memMiB }

// placePreferring is place, but prefer if it fits.
func placePreferring(nodes []NodeInfo, memMiB int, prefer string) string {
	for _, nd := range nodes {
		if nd.Name == prefer && nd.fits(memMiB) {
			return prefer
		}
	}
	return place(nodes, memMiB)
}

func findNode(nodes []NodeInfo, name string) *NodeInfo {
	for i := range nodes {
		if nodes[i].Name == name {
			return &nodes[i]
		}
	}
	return nil
}

func without(nodes []NodeInfo, name string) []NodeInfo {
	var out []NodeInfo
	for _, nd := range nodes {
		if nd.Name != name {
			out = append(out, nd)
		}
	}
	return out
}

func only(nodes []NodeInfo, name string) []NodeInfo {
	if nd := findNode(nodes, name); nd != nil {
		return []NodeInfo{*nd}
	}
	return nil
}
