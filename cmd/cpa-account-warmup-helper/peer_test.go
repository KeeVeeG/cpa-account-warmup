package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"cpa-account-warmup/internal/plugin"
)

type testHandler struct {
	handle func(string, []byte) []byte
	close  func()
}

func (h testHandler) Handle(method string, raw []byte) []byte { return h.handle(method, raw) }
func (h testHandler) Close() {
	if h.close != nil {
		h.close()
	}
}

func startPeer(t *testing.T, makeHandler func(*peer) rpcHandler) (*peer, net.Conn, <-chan error) {
	t.Helper()
	client, server := net.Pipe()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	p := newPeer(server)
	p.handler = makeHandler(p)
	done := make(chan error, 1)
	go func() { done <- p.serve() }()
	t.Cleanup(func() {
		_ = client.Close()
		p.disconnect()
	})
	return p, client, done
}

func sendFrame(t *testing.T, conn io.Writer, f frame) {
	t.Helper()
	if err := writeFrame(conn, f); err != nil {
		t.Fatal(err)
	}
}

func receiveFrame(t *testing.T, conn io.Reader) frame {
	t.Helper()
	f, err := readFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func waitForExit(t *testing.T, done <-chan error, wantError bool) {
	t.Helper()
	select {
	case err := <-done:
		if (err != nil) != wantError {
			t.Fatalf("serve() = %v, want error %t", err, wantError)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not exit")
	}
}

func requestFrame(id uint64, method string) frame {
	return frame{kind: pluginRequest, id: id, payload: []byte(method + "\x00{}")}
}

func TestNestedHostCallbackWhilePluginRequestWaits(t *testing.T) {
	_, conn, done := startPeer(t, func(p *peer) rpcHandler {
		return testHandler{handle: func(method string, _ []byte) []byte {
			if method == "outer" {
				var result struct{ Value string }
				if p.Call("host.test", struct{}{}, &result) != nil {
					return []byte(`{"ok":false}`)
				}
				value, _ := json.Marshal(result)
				return value
			}
			return []byte(`{"nested":true}`)
		}}
	})
	sendFrame(t, conn, requestFrame(101, "outer"))
	call := receiveFrame(t, conn)
	if call.kind != hostRequest || !bytes.Equal(call.payload, []byte("host.test\x00{}")) {
		t.Fatalf("unexpected host request: %+v", call)
	}
	sendFrame(t, conn, requestFrame(102, "executor.execute"))
	nested := receiveFrame(t, conn)
	if nested.kind != pluginResponse || nested.id != 102 || string(nested.payload) != `{"nested":true}` {
		t.Fatalf("nested callback failed: %+v", nested)
	}
	sendFrame(t, conn, frame{kind: hostResponse, id: call.id, payload: []byte(`{"ok":true,"result":{"Value":"returned"}}`)})
	outer := receiveFrame(t, conn)
	if outer.id != 101 || string(outer.payload) != `{"Value":"returned"}` {
		t.Fatalf("outer callback failed: %+v", outer)
	}
	sendFrame(t, conn, frame{kind: shutdown, id: 103})
	if reply := receiveFrame(t, conn); reply.id != 103 || reply.status != 0 {
		t.Fatalf("shutdown failed: %+v", reply)
	}
	waitForExit(t, done, false)
}

func TestConcurrentHostCallsMatchOutOfOrderReplies(t *testing.T) {
	p, conn, _ := startPeer(t, func(*peer) rpcHandler { return testHandler{} })
	results := make(chan error, 2)
	for _, value := range []string{"first", "second"} {
		go func() {
			var result string
			err := p.Call("host."+value, value, &result)
			if err == nil && result != value {
				err = errors.New("response reached the wrong caller")
			}
			results <- err
		}()
	}
	first, second := receiveFrame(t, conn), receiveFrame(t, conn)
	if first.id == second.id {
		t.Fatal("concurrent host requests reused an ID")
	}
	for _, request := range []frame{second, first} {
		_, raw, valid := decodeRequest(request.payload)
		if !valid {
			t.Fatal("malformed host request")
		}
		body := append([]byte(`{"ok":true,"result":`), raw...)
		body = append(body, '}')
		sendFrame(t, conn, frame{kind: hostResponse, id: request.id, payload: body})
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestShutdownDrainsActiveRequestsAndAllowsNestedCallbacks(t *testing.T) {
	closing := make(chan struct{})
	finishClose := make(chan struct{})
	outerStarted := make(chan struct{})
	finishOuter := make(chan struct{})
	_, conn, done := startPeer(t, func(*peer) rpcHandler {
		return testHandler{
			handle: func(method string, _ []byte) []byte {
				if method == "management.handle" {
					close(outerStarted)
					<-finishOuter
				}
				return []byte(`{"ok":true}`)
			},
			close: func() { close(closing); <-finishClose },
		}
	})
	sendFrame(t, conn, requestFrame(1, "management.handle"))
	<-outerStarted
	sendFrame(t, conn, frame{kind: shutdown, id: 2})
	<-closing
	sendFrame(t, conn, requestFrame(3, "management.handle"))
	if denied := receiveFrame(t, conn); denied.id != 3 || denied.status != 1 {
		t.Fatalf("shutdown admitted a management request: %+v", denied)
	}
	sendFrame(t, conn, requestFrame(4, "executor.execute"))
	if nested := receiveFrame(t, conn); nested.id != 4 || nested.status != 0 {
		t.Fatalf("shutdown rejected a nested callback: %+v", nested)
	}
	close(finishClose)
	close(finishOuter)
	if outer := receiveFrame(t, conn); outer.id != 1 {
		t.Fatalf("shutdown completed before the outer response: %+v", outer)
	}
	if reply := receiveFrame(t, conn); reply.id != 2 || reply.status != 0 {
		t.Fatalf("invalid shutdown response: %+v", reply)
	}
	waitForExit(t, done, false)
}

func TestDisconnectReleasesWaitingHostCall(t *testing.T) {
	p, conn, done := startPeer(t, func(*peer) rpcHandler { return testHandler{} })
	result := make(chan error, 1)
	go func() { result <- p.Call("host.test", struct{}{}, &struct{}{}) }()
	receiveFrame(t, conn)
	_ = conn.Close()
	waitForExit(t, done, true)
	select {
	case err := <-result:
		if !errors.Is(err, errDisconnected) {
			t.Fatalf("unexpected host-call error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("host call remained blocked after EOF")
	}
}

func TestHostErrorRetainsStatusWithoutSensitiveContent(t *testing.T) {
	err := decodeHostResult([]byte(`{"ok":false,"error":{"http_status":429,"message":"secret-upstream-token"}}`), 1, &struct{}{})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != 429 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe or incomplete host error: %v", err)
	}
	for _, raw := range []string{"", "invalid JSON", `{"ok":true,"result":"wrong type"}`} {
		if decodeHostResult([]byte(raw), 0, &struct{}{}) == nil {
			t.Fatalf("accepted malformed host response %q", raw)
		}
	}
}

func TestRealPluginInitializesAndCloses(t *testing.T) {
	_, conn, done := startPeer(t, func(p *peer) rpcHandler { return plugin.New(p) })
	sendFrame(t, conn, requestFrame(1, "plugin.register"))
	reply := receiveFrame(t, conn)
	var registration struct {
		OK     bool `json:"ok"`
		Result struct {
			Metadata struct {
				Name string `json:"Name"`
			} `json:"metadata"`
		} `json:"result"`
	}
	if json.Unmarshal(reply.payload, &registration) != nil || !registration.OK || reply.status != 0 || registration.Result.Metadata.Name != "Account Warmup" {
		t.Fatalf("real plugin did not initialize: %s", reply.payload)
	}
	sendFrame(t, conn, requestFrame(2, "management.register"))
	resources := receiveFrame(t, conn)
	if resources.status != 0 || !bytes.Contains(resources.payload, []byte("Account Warmup")) {
		t.Fatalf("management resources were not registered: %s", resources.payload)
	}
	sendFrame(t, conn, frame{kind: shutdown, id: 3})
	if reply := receiveFrame(t, conn); reply.id != 3 || reply.status != 0 {
		t.Fatalf("real plugin did not drain: %+v", reply)
	}
	waitForExit(t, done, false)
}

func TestDuplicatePluginRequestTerminatesTransport(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	defer close(finish)
	_, conn, done := startPeer(t, func(*peer) rpcHandler {
		return testHandler{handle: func(string, []byte) []byte {
			close(started)
			<-finish
			return []byte(`{"ok":true}`)
		}}
	})
	sendFrame(t, conn, requestFrame(1, "test"))
	<-started
	sendFrame(t, conn, requestFrame(1, "test"))
	waitForExit(t, done, true)
}

func TestUnexpectedResponseTerminatesTransport(t *testing.T) {
	for _, kind := range []uint32{hostResponse, hostRequest, pluginResponse} {
		t.Run(string(rune('0'+kind)), func(t *testing.T) {
			_, conn, done := startPeer(t, func(*peer) rpcHandler { return testHandler{} })
			sendFrame(t, conn, frame{kind: kind, id: 1, payload: []byte(`{}`)})
			waitForExit(t, done, true)
		})
	}
}

func TestProtocolRejectsMalformedFrames(t *testing.T) {
	for _, mutation := range []func([]byte){
		func(header []byte) { binary.LittleEndian.PutUint32(header[:4], 0) },
		func(header []byte) { binary.LittleEndian.PutUint32(header[4:8], 99) },
		func(header []byte) { binary.LittleEndian.PutUint64(header[8:16], 0) },
		func(header []byte) { binary.LittleEndian.PutUint32(header[16:20], 1) },
		func(header []byte) { binary.LittleEndian.PutUint32(header[20:24], maxRPCBytes+1) },
	} {
		var encoded bytes.Buffer
		if err := writeFrame(&encoded, requestFrame(1, "test")); err != nil {
			t.Fatal(err)
		}
		mutation(encoded.Bytes())
		if _, err := readFrame(&encoded); !errors.Is(err, errProtocol) {
			t.Fatalf("malformed header was accepted: %v", err)
		}
	}
	for _, payload := range [][]byte{nil, []byte("no-separator"), []byte("\x00{}"), []byte("bad\nmethod\x00{}"), {0xff, 0, '{', '}'}} {
		if _, _, valid := decodeRequest(payload); valid {
			t.Fatalf("malformed method was accepted: %q", payload)
		}
	}
}
