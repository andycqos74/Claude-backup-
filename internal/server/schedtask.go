package server

import (
	"context"
	"fmt"
	"sync"

	"centralbackup/internal/proto"
)

// Scheduled-task management mirrors Docker discovery: the GUI asks an agent
// for its task list, or asks it to act on one task, over the control
// channel, correlated by request ID. Two reply types (inventory and action
// result) are tracked separately but with the same pattern.

type schedTasks struct {
	mu         sync.Mutex
	pendingInv map[string]chan proto.SchedInventory
	pendingRes map[string]chan proto.SchedResult
}

func (d *schedTasks) beginInv(id string) chan proto.SchedInventory {
	ch := make(chan proto.SchedInventory, 1)
	d.mu.Lock()
	if d.pendingInv == nil {
		d.pendingInv = map[string]chan proto.SchedInventory{}
	}
	d.pendingInv[id] = ch
	d.mu.Unlock()
	return ch
}

func (d *schedTasks) endInv(id string) {
	d.mu.Lock()
	delete(d.pendingInv, id)
	d.mu.Unlock()
}

func (d *schedTasks) deliverInv(inv proto.SchedInventory) {
	d.mu.Lock()
	ch := d.pendingInv[inv.RequestID]
	d.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- inv:
	default:
	}
}

func (d *schedTasks) beginRes(id string) chan proto.SchedResult {
	ch := make(chan proto.SchedResult, 1)
	d.mu.Lock()
	if d.pendingRes == nil {
		d.pendingRes = map[string]chan proto.SchedResult{}
	}
	d.pendingRes[id] = ch
	d.mu.Unlock()
	return ch
}

func (d *schedTasks) endRes(id string) {
	d.mu.Lock()
	delete(d.pendingRes, id)
	d.mu.Unlock()
}

func (d *schedTasks) deliverRes(res proto.SchedResult) {
	d.mu.Lock()
	ch := d.pendingRes[res.RequestID]
	d.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- res:
	default:
	}
}

// listSchedTasks asks an online agent for its scheduled-task inventory.
func (s *Server) listSchedTasks(ctx context.Context, agentID string) (proto.SchedInventory, error) {
	if !s.hub.Online(agentID) {
		return proto.SchedInventory{}, fmt.Errorf("client is offline")
	}
	id := newRequestID()
	ch := s.sched.beginInv(id)
	defer s.sched.endInv(id)

	if !s.hub.Send(agentID, proto.MsgListSched, proto.ListSched{RequestID: id}) {
		return proto.SchedInventory{}, fmt.Errorf("could not reach the client")
	}
	select {
	case inv := <-ch:
		return inv, nil
	case <-ctx.Done():
		return proto.SchedInventory{}, fmt.Errorf("the client did not respond in time")
	}
}

// doSchedAction sends one scheduled-task action to an agent and waits for
// its result. The action is validated here as well as on the agent, so the
// server never forwards anything outside the allowed operation set.
func (s *Server) doSchedAction(ctx context.Context, agentID string, act proto.SchedAction) error {
	switch act.Action {
	case proto.SchedActionRun, proto.SchedActionEnable, proto.SchedActionDisable, proto.SchedActionSetSchedule:
	default:
		return fmt.Errorf("unsupported action")
	}
	if !s.hub.Online(agentID) {
		return fmt.Errorf("client is offline")
	}
	act.RequestID = newRequestID()
	ch := s.sched.beginRes(act.RequestID)
	defer s.sched.endRes(act.RequestID)

	if !s.hub.Send(agentID, proto.MsgSchedAction, act) {
		return fmt.Errorf("could not reach the client")
	}
	select {
	case res := <-ch:
		if !res.OK {
			if res.Error == "" {
				res.Error = "the client reported an error"
			}
			return fmt.Errorf("%s", res.Error)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("the client did not respond in time")
	}
}
