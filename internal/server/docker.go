package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"

	"centralbackup/internal/proto"
)

// Docker discovery: the job editor asks an agent what containers exist on
// its host so they can be ticked instead of typed. The control channel is
// otherwise fire-and-forget, so this adds the one request/response exchange
// the system needs, correlated by request ID.

type dockerDiscovery struct {
	mu      sync.Mutex
	pending map[string]chan proto.DockerInventory
}

func (d *dockerDiscovery) begin(id string) chan proto.DockerInventory {
	ch := make(chan proto.DockerInventory, 1)
	d.mu.Lock()
	if d.pending == nil {
		d.pending = map[string]chan proto.DockerInventory{}
	}
	d.pending[id] = ch
	d.mu.Unlock()
	return ch
}

func (d *dockerDiscovery) end(id string) {
	d.mu.Lock()
	delete(d.pending, id)
	d.mu.Unlock()
}

// deliver hands an inventory to whoever is waiting for it. Unknown request
// IDs are dropped: the waiter has already timed out and gone away.
func (d *dockerDiscovery) deliver(inv proto.DockerInventory) {
	d.mu.Lock()
	ch := d.pending[inv.RequestID]
	d.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- inv:
	default:
	}
}

// discoverDocker asks an online agent for its container inventory and waits
// for the reply.
func (s *Server) discoverDocker(ctx context.Context, agentID string) (proto.DockerInventory, error) {
	if !s.hub.Online(agentID) {
		return proto.DockerInventory{}, fmt.Errorf("client is offline")
	}
	id := newRequestID()
	ch := s.docker.begin(id)
	defer s.docker.end(id)

	if !s.hub.Send(agentID, proto.MsgDiscoverDocker, proto.DiscoverDocker{RequestID: id}) {
		return proto.DockerInventory{}, fmt.Errorf("could not reach the client")
	}
	select {
	case inv := <-ch:
		return inv, nil
	case <-ctx.Done():
		return proto.DockerInventory{}, fmt.Errorf("the client did not respond in time")
	}
}

func newRequestID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
