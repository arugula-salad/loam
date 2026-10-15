package nodelet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/arugula-salad/loam/internal/nodeapi"
	"github.com/arugula-salad/loam/internal/nodelet/firecracker"
)

// Machine is a running VM as the nodelet drives it. *firecracker.Machine is
// the one that runs; tests use a fake.
type Machine interface {
	// Dial opens a stream to the guest's vsock port.
	Dial(ctx context.Context, port uint32) (net.Conn, error)
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	// Squeeze inflates the balloon over the guest's free memory; SetBalloon sets it.
	Squeeze(ctx context.Context) (int, error)
	SetBalloon(ctx context.Context, mib int) error
	// Snapshot writes the paused VM's state and memory to files (absolute
	// paths): the whole memory, or with diff only the pages dirtied since the
	// VM was restored or last snapshotted (firecracker.Machine.Snapshot).
	Snapshot(ctx context.Context, state, mem string, diff bool) error
	Kill()
	Exited() <-chan struct{}
}

// VMM starts machines.
type VMM interface {
	Boot(ctx context.Context, cfg firecracker.Config) (Machine, error)
	Restore(ctx context.Context, cfg firecracker.Config, state string, mem firecracker.Memory) (Machine, error)
	// ReapOrphan kills a VMM a previous nodelet left running in dir.
	ReapOrphan(dir string)
}

// Firecracker is the VMM that runs.
type Firecracker struct{ Host firecracker.Host }

func (f Firecracker) Boot(ctx context.Context, cfg firecracker.Config) (Machine, error) {
	m, err := firecracker.Boot(ctx, f.Host, cfg)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (f Firecracker) Restore(ctx context.Context, cfg firecracker.Config, state string, mem firecracker.Memory) (Machine, error) {
	m, err := firecracker.Restore(ctx, f.Host, cfg, state, mem)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (Firecracker) ReapOrphan(dir string) { firecracker.ReapOrphan(dir) }

// The vsock ports (docs/AGENT.md, docs/NODE.md).
const (
	AgentPort = 1024 // the agent's HTTP API, in the guest
	HostPort  = 1025 // the guest's host channel, on the host
)

// errNoRoute is the agent answering 404: a route it doesn't have.
var errNoRoute = errors.New("the agent has no such route")

// agentCall makes one HTTP request to the guest agent over a fresh stream.
func agentCall(ctx context.Context, m Machine, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := nodeapi.AgentTransport(func(ctx context.Context) (net.Conn, error) { return m.Dial(ctx, AgentPort) }).RoundTrip(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("agent %s %s: %w", method, path, errNoRoute)
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("agent %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
