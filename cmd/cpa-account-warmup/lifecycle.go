package main

import "sync"

type rpcHandler interface {
	Handle(string, []byte) []byte
	Close()
}

// Draining must allow nested host callbacks: an active warmup can re-enter the
// plugin through the core's authenticated API before its outer call finishes.
type rpcGate struct {
	mu      sync.Mutex
	changed *sync.Cond
	handler rpcHandler
	active  int
	closing bool
}

func newRPCGate() *rpcGate {
	gate := &rpcGate{}
	gate.changed = sync.NewCond(&gate.mu)
	return gate
}

func (g *rpcGate) open(handler rpcHandler) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.handler, g.closing = handler, false
}

func (g *rpcGate) acquire(method string) rpcHandler {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.handler == nil || (g.closing && !isDrainCallback(method)) {
		return nil
	}
	g.active++
	return g.handler
}

func (g *rpcGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.active == 0 {
		g.changed.Broadcast()
	}
}

// The caller serializes open and close across native initialization/shutdown.
func (g *rpcGate) close() {
	g.mu.Lock()
	handler := g.handler
	if handler == nil {
		g.mu.Unlock()
		return
	}
	g.closing = true
	g.mu.Unlock()
	handler.Close()
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.active != 0 {
		g.changed.Wait()
	}
	g.handler = nil
}

func isDrainCallback(method string) bool {
	switch method {
	case "frontend_auth.identifier", "frontend_auth.authenticate", "model.route", "executor.identifier", "executor.execute":
		return true
	default:
		return false
	}
}
