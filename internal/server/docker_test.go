package server

import (
	"context"
	"testing"
	"time"

	"centralbackup/internal/proto"
)

func TestDockerDiscoveryDeliversToWaiter(t *testing.T) {
	var d dockerDiscovery
	ch := d.begin("req-1")
	defer d.end("req-1")

	want := proto.DockerInventory{
		RequestID: "req-1",
		Available: true,
		HostRoot:  "/host",
		Containers: []proto.DockerContainer{
			{Name: "mysql", Kind: proto.DockerKindDatabase, Engine: "mysql"},
		},
	}
	go d.deliver(want)

	select {
	case got := <-ch:
		if !got.Available || len(got.Containers) != 1 || got.Containers[0].Name != "mysql" {
			t.Errorf("delivered inventory = %+v, want the one that was sent", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inventory was never delivered to the waiter")
	}
}

func TestDockerDiscoveryDropsUnknownAndStaleRequests(t *testing.T) {
	var d dockerDiscovery

	// A reply for a request nobody is waiting on must not panic — this is
	// the normal case after a waiter times out.
	d.deliver(proto.DockerInventory{RequestID: "never-registered", Available: true})

	// A reply arriving after end() is likewise dropped rather than leaking.
	ch := d.begin("req-2")
	d.end("req-2")
	d.deliver(proto.DockerInventory{RequestID: "req-2", Available: true})
	select {
	case <-ch:
		t.Error("a reply was delivered to an ended request")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestDiscoverDockerOfflineAgent(t *testing.T) {
	s := &Server{hub: newHub()}
	_, err := s.discoverDocker(context.Background(), "no-such-agent")
	if err == nil {
		t.Fatal("expected an error for an offline agent")
	}
}

// A slow or unresponsive agent must not hang the admin request: the context
// deadline has to win, and the pending entry must not be left behind.
func TestDiscoverDockerTimesOut(t *testing.T) {
	var d dockerDiscovery
	ch := d.begin("slow")
	defer d.end("slow")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	select {
	case <-ch:
		t.Fatal("nothing should have been delivered")
	case <-ctx.Done():
	}

	d.end("slow")
	d.mu.Lock()
	n := len(d.pending)
	d.mu.Unlock()
	if n != 0 {
		t.Errorf("pending requests after end() = %d, want 0", n)
	}
}
