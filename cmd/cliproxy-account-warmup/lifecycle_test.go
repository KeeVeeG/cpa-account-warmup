package main

import (
	"testing"
	"time"
)

type drainingHandler struct {
	closing chan struct{}
	finish  chan struct{}
}

func (*drainingHandler) Handle(string, []byte) []byte { return nil }

func (h *drainingHandler) Close() {
	close(h.closing)
	<-h.finish
}

func TestShutdownAllowsNestedCallbacksUntilOuterCallFinishes(t *testing.T) {
	gate := newRPCGate()
	handler := &drainingHandler{closing: make(chan struct{}), finish: make(chan struct{})}
	gate.open(handler)
	if gate.acquire("management.handle") == nil {
		t.Fatal("the outer request was rejected")
	}
	done := make(chan struct{})
	go func() {
		gate.close()
		close(done)
	}()
	<-handler.closing
	if gate.acquire("management.handle") != nil {
		gate.release()
		t.Fatal("shutdown admitted another management request")
	}
	for _, method := range []string{"frontend_auth.identifier", "frontend_auth.authenticate", "model.route", "executor.identifier", "executor.execute"} {
		if gate.acquire(method) == nil {
			t.Fatalf("shutdown blocked the nested %s callback", method)
		}
		gate.release()
	}
	close(handler.finish)
	select {
	case <-done:
		t.Fatal("shutdown finished before the outer request")
	default:
	}
	gate.release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown deadlocked after all requests finished")
	}
	if gate.acquire("executor.execute") != nil {
		gate.release()
		t.Fatal("a callback acquired the closed handler")
	}
}
