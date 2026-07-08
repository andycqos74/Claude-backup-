package server

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"centralbackup/internal/proto"
)

// Hub tracks the live WebSocket connection of every online agent. Agents
// connect outbound to the server; the hub is how the server pushes commands
// (run backup, restore, job updates) down to them.
type Hub struct {
	mu    sync.Mutex
	conns map[string]*agentConn
}

func newHub() *Hub {
	return &Hub{conns: map[string]*agentConn{}}
}

type agentConn struct {
	agentID string
	ws      *websocket.Conn
	send    chan proto.Envelope
	done    chan struct{}
	once    sync.Once
}

func (c *agentConn) close() {
	c.once.Do(func() {
		close(c.done)
		c.ws.Close()
	})
}

// register adds a connection, replacing (and closing) any previous
// connection for the same agent. It starts the writer pump.
func (h *Hub) register(agentID string, ws *websocket.Conn) *agentConn {
	c := &agentConn{
		agentID: agentID,
		ws:      ws,
		send:    make(chan proto.Envelope, 64),
		done:    make(chan struct{}),
	}
	h.mu.Lock()
	if old := h.conns[agentID]; old != nil {
		old.close()
	}
	h.conns[agentID] = c
	h.mu.Unlock()

	go func() {
		ping := time.NewTicker(30 * time.Second)
		defer ping.Stop()
		for {
			select {
			case env := <-c.send:
				ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
				if err := ws.WriteJSON(env); err != nil {
					c.close()
					return
				}
			case <-ping.C:
				ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
				if err := ws.WriteMessage(websocket.PingMessage, nil); err != nil {
					c.close()
					return
				}
			case <-c.done:
				return
			}
		}
	}()
	return c
}

// unregister removes the connection if it is still the current one.
func (h *Hub) unregister(c *agentConn) {
	h.mu.Lock()
	if h.conns[c.agentID] == c {
		delete(h.conns, c.agentID)
	}
	h.mu.Unlock()
	c.close()
}

// Online reports whether an agent currently has a live connection.
func (h *Hub) Online(agentID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[agentID] != nil
}

// OnlineIDs returns the set of currently connected agent IDs.
func (h *Hub) OnlineIDs() map[string]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]bool, len(h.conns))
	for id := range h.conns {
		out[id] = true
	}
	return out
}

// Send marshals payload and queues it for the agent. Returns false if the
// agent is offline or its send queue is saturated.
func (h *Hub) Send(agentID, msgType string, payload any) bool {
	env, err := proto.Wrap(msgType, payload)
	if err != nil {
		log.Printf("hub: marshal %s: %v", msgType, err)
		return false
	}
	h.mu.Lock()
	c := h.conns[agentID]
	h.mu.Unlock()
	if c == nil {
		return false
	}
	select {
	case c.send <- env:
		return true
	case <-c.done:
		return false
	case <-time.After(10 * time.Second):
		log.Printf("hub: send queue full for agent %s, dropping %s", agentID, msgType)
		return false
	}
}

func unmarshal[T any](data json.RawMessage) (T, error) {
	var v T
	err := json.Unmarshal(data, &v)
	return v, err
}
