package server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"centralbackup/internal/proto"
)

// Remote file browser: the server asks an online agent to list one directory
// on the client so the GUI can navigate its filesystem and pick files to
// push or pull. Same correlated request/response shape as Docker discovery —
// the control channel is otherwise fire-and-forget.

type browseRequests struct {
	mu      sync.Mutex
	pending map[string]chan proto.DirListing
}

func (b *browseRequests) begin(id string) chan proto.DirListing {
	ch := make(chan proto.DirListing, 1)
	b.mu.Lock()
	if b.pending == nil {
		b.pending = map[string]chan proto.DirListing{}
	}
	b.pending[id] = ch
	b.mu.Unlock()
	return ch
}

func (b *browseRequests) end(id string) {
	b.mu.Lock()
	delete(b.pending, id)
	b.mu.Unlock()
}

// deliver hands a listing to whoever is waiting for it. Unknown request IDs
// are dropped: the waiter has already timed out and gone away.
func (b *browseRequests) deliver(l proto.DirListing) {
	b.mu.Lock()
	ch := b.pending[l.RequestID]
	b.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- l:
	default:
	}
}

// browseDir asks an online agent to list a directory and waits for the reply.
func (s *Server) browseDir(ctx context.Context, agentID, path string) (proto.DirListing, error) {
	if !s.hub.Online(agentID) {
		return proto.DirListing{}, fmt.Errorf("client is offline")
	}
	id := newRequestID()
	ch := s.browse.begin(id)
	defer s.browse.end(id)

	if !s.hub.Send(agentID, proto.MsgBrowseDir, proto.BrowseDir{RequestID: id, Path: path}) {
		return proto.DirListing{}, fmt.Errorf("could not reach the client")
	}
	select {
	case l := <-ch:
		return l, nil
	case <-ctx.Done():
		return proto.DirListing{}, fmt.Errorf("the client did not respond in time")
	}
}

// handleAgentBrowse lists a directory on the client for the GUI file browser.
func (s *Server) handleAgentBrowse(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	listing, err := s.browseDir(ctx, r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		httpError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, listing)
}
