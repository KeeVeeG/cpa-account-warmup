package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"cpa-account-warmup/internal/plugin"
)

var errDisconnected = errors.New("Plugin host disconnected; upstream completion may be unknown.")

type rpcHandler interface {
	Handle(string, []byte) []byte
	Close()
}

type hostResult struct {
	frame
	err error
}

type peer struct {
	conn       io.ReadWriteCloser
	handler    rpcHandler
	writeMu    sync.Mutex
	mu         sync.Mutex
	changed    *sync.Cond
	pending    map[uint64]chan hostResult
	active     map[uint64]struct{}
	nextID     uint64
	shutdownID uint64
	closing    bool
	closed     bool
	drained    bool
}

func newPeer(conn io.ReadWriteCloser) *peer {
	p := &peer{conn: conn, pending: make(map[uint64]chan hostResult), active: make(map[uint64]struct{})}
	p.changed = sync.NewCond(&p.mu)
	return p
}

func (p *peer) serve() error {
	defer p.disconnect()
	for {
		f, err := readFrame(p.conn)
		if err != nil {
			p.mu.Lock()
			drained := p.drained
			p.mu.Unlock()
			if drained {
				return nil
			}
			return err
		}
		switch f.kind {
		case pluginRequest:
			method, raw, valid := decodeRequest(f.payload)
			if !valid {
				return errProtocol
			}
			p.mu.Lock()
			_, duplicate := p.active[f.id]
			if duplicate || p.drained || f.id == p.shutdownID {
				p.mu.Unlock()
				return errProtocol
			}
			p.active[f.id] = struct{}{}
			admitted := !p.closing || isDrainCallback(method)
			p.mu.Unlock()
			go p.handle(f.id, method, raw, admitted)
		case hostResponse:
			p.mu.Lock()
			result, ok := p.pending[f.id]
			delete(p.pending, f.id)
			p.mu.Unlock()
			if !ok {
				return errProtocol
			}
			result <- hostResult{frame: f}
		case shutdown:
			p.mu.Lock()
			_, duplicate := p.active[f.id]
			if p.closing || duplicate {
				p.mu.Unlock()
				return errProtocol
			}
			p.closing = true
			p.shutdownID = f.id
			p.mu.Unlock()
			go p.drain(f.id)
		default:
			return errProtocol
		}
	}
}

func decodeRequest(payload []byte) (string, []byte, bool) {
	separator := bytes.IndexByte(payload, 0)
	if separator <= 0 || !validMethod(string(payload[:separator])) {
		return "", nil, false
	}
	method := string(payload[:separator])
	return method, payload[separator+1:], true
}

func validMethod(method string) bool {
	if len(method) == 0 || len(method) > 1024 {
		return false
	}
	for i := range len(method) {
		if method[i] < 0x21 || method[i] > 0x7e {
			return false
		}
	}
	return true
}

func (p *peer) handle(id uint64, method string, raw []byte, admitted bool) {
	response := frame{kind: pluginResponse, id: id}
	defer func() {
		if recover() != nil {
			response.status = 1
			response.payload = plugin.ErrorEnvelope("internal_error", "The plugin could not complete this request.")
		}
		if len(response.payload) == 0 || len(response.payload) > maxRPCBytes {
			response.status = 1
			response.payload = plugin.ErrorEnvelope("invalid_response", "Invalid plugin RPC response.")
		}
		_ = p.send(response)
		p.mu.Lock()
		delete(p.active, id)
		p.changed.Broadcast()
		p.mu.Unlock()
	}()
	if !admitted {
		response.status = 1
		response.payload = plugin.ErrorEnvelope("not_initialized", "The plugin is shutting down.")
		return
	}
	response.payload = p.handler.Handle(method, raw)
}

// Keep reading while Close drains: a warmup can re-enter through the core's
// authenticated API and needs those nested callbacks to finish.
func (p *peer) drain(id uint64) {
	defer func() {
		if recover() != nil {
			p.disconnect()
		}
	}()
	p.handler.Close()
	p.mu.Lock()
	for len(p.active) != 0 && !p.closed {
		p.changed.Wait()
	}
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.drained = true
	p.mu.Unlock()
	_ = p.send(frame{kind: pluginResponse, id: id, payload: []byte(`{"ok":true,"result":{}}`)})
	p.disconnect()
}

func isDrainCallback(method string) bool {
	switch method {
	case "frontend_auth.identifier", "frontend_auth.authenticate", "model.route", "executor.identifier", "executor.execute":
		return true
	default:
		return false
	}
}

func (p *peer) send(f frame) error {
	p.writeMu.Lock()
	err := writeFrame(p.conn, f)
	p.writeMu.Unlock()
	if err != nil {
		p.disconnect()
	}
	return err
}

func (p *peer) disconnect() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	for id, result := range p.pending {
		result <- hostResult{err: errDisconnected}
		delete(p.pending, id)
	}
	p.changed.Broadcast()
	p.mu.Unlock()
	_ = p.conn.Close()
}

func (p *peer) Call(method string, request any, target any) error {
	raw, err := json.Marshal(request)
	if err != nil {
		return errors.New("Could not encode host request.")
	}
	if !validMethod(method) || len(method)+1+len(raw) > maxRPCBytes {
		return errors.New("Invalid host request.")
	}
	p.mu.Lock()
	if p.closed || p.drained {
		p.mu.Unlock()
		return errDisconnected
	}
	p.nextID++
	if p.nextID == 0 {
		p.mu.Unlock()
		return errors.New("Host request IDs exhausted.")
	}
	id := p.nextID
	result := make(chan hostResult, 1)
	p.pending[id] = result
	p.mu.Unlock()
	payload := make([]byte, 0, len(method)+1+len(raw))
	payload = append(payload, method...)
	payload = append(payload, 0)
	payload = append(payload, raw...)
	if err := p.send(frame{kind: hostRequest, id: id, payload: payload}); err != nil {
		return errDisconnected
	}
	response := <-result
	if response.err != nil {
		return response.err
	}
	return decodeHostResult(response.payload, response.status, target)
}

type hostCallError struct{ statusCode int }

func (e hostCallError) Error() string {
	return "Host callback failed; upstream completion may be unknown."
}

func (e hostCallError) StatusCode() int { return e.statusCode }

func decodeHostResult(raw []byte, code uint32, target any) error {
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			HTTPStatus int `json:"http_status"`
		} `json:"error"`
	}
	// Upstream error text can contain credentials; only retain the HTTP status.
	if json.Unmarshal(raw, &env) != nil {
		return hostCallError{}
	}
	if code != 0 || !env.OK {
		status := 0
		if env.Error != nil && env.Error.HTTPStatus >= 400 && env.Error.HTTPStatus <= 599 {
			status = env.Error.HTTPStatus
		}
		return hostCallError{statusCode: status}
	}
	if json.Unmarshal(env.Result, target) != nil {
		return errors.New("Invalid host result.")
	}
	return nil
}
