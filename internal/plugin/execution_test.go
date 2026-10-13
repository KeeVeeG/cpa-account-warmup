package plugin

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-account-warmup/internal/warmup"
)

type executionTestHost struct {
	mu       sync.Mutex
	requests []executionRequest
	err      error
	panic    bool
	entered  chan struct{}
	release  chan struct{}
}

func (h *executionTestHost) Call(method string, input, target any) error {
	if method != "host.model.execute" {
		return errors.New("Unexpected host method")
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	var request executionRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return err
	}
	h.mu.Lock()
	h.requests = append(h.requests, request)
	h.mu.Unlock()
	if h.entered != nil {
		h.entered <- struct{}{}
	}
	if h.release != nil {
		<-h.release
	}
	if h.panic {
		panic("private upstream content")
	}
	if h.err != nil {
		return h.err
	}
	raw, err = json.Marshal(executionResponse{StatusCode: 200, Body: []byte(`{"result":"native response"}`)})
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func preparedExecution(provider string) executionRequest {
	protocol, model := "openai-response", "gpt-6-luna"
	if provider == "claude" {
		protocol, model = "claude", "claude-haiku-5-5"
	}
	body, _ := json.Marshal(map[string]any{"model": model, "input": "Reply OK", "stream": false})
	return executionRequest{
		EntryProtocol: protocol, ExitProtocol: protocol, Model: model,
		AuthID: "exact-account", ForcedProvider: provider,
		Headers: http.Header{"Content-Type": {"application/json"}, "X-Cliproxyapi-Account-Warmup": {"true"}},
		Body:    body, HostCallbackID: "old-management-context",
	}
}

func rpcResult(t *testing.T, server *Server, method string, input, output any) *RPCError {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Error(err)
		return &RPCError{HTTPStatus: 500}
	}
	var envelope struct {
		OK     bool
		Result json.RawMessage
		Error  *RPCError
	}
	if err := json.Unmarshal(server.Handle(method, raw), &envelope); err != nil {
		t.Error(err)
		return &RPCError{HTTPStatus: 500}
	}
	if envelope.Error != nil {
		return envelope.Error
	}
	if !envelope.OK {
		t.Error("RPC did not return an outcome")
		return &RPCError{HTTPStatus: 500}
	}
	if err := json.Unmarshal(envelope.Result, output); err != nil {
		t.Error(err)
		return &RPCError{HTTPStatus: 500}
	}
	return nil
}

func fakeHistoryCore(t *testing.T, server *Server) *httptest.Server {
	t.Helper()
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
		var auth frontendAuthResponse
		if rpcResult(t, server, "frontend_auth.authenticate", frontendAuthRequest{Method: r.Method, Path: r.URL.Path, Headers: r.Header, Query: r.URL.Query(), Body: body}, &auth) != nil || !auth.Authenticated {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if auth.Principal != "Account Warmup" || strings.Contains(auth.Principal, executionPrefix) {
			t.Error("The execution secret became the Usage identity")
		}
		var model struct{ Model string }
		_ = json.Unmarshal(body, &model)
		var route routeResponse
		if rpcResult(t, server, "model.route", routeRequest{RequestedModel: model.Model, Headers: r.Header}, &route) != nil || !route.Handled || route.TargetKind != "self" {
			t.Error("Warmup escaped its plugin executor")
			w.WriteHeader(500)
			return
		}
		protocol := "openai-response"
		if r.URL.Path == "/v1/messages" {
			protocol = "claude"
		}
		input := executorRequest{
			SourceFormat: protocol, Model: model.Model, Headers: r.Header,
			OriginalRequest: body, Payload: body, Metadata: map[string]any{"request_path": r.URL.Path},
			HostCallbackID: "public-api-callback",
		}
		var output executorResponse
		if failure := rpcResult(t, server, "executor.execute", input, &output); failure != nil {
			w.WriteHeader(failure.HTTPStatus)
			return
		}
		_, _ = w.Write(output.Payload)
	}))
	t.Cleanup(core.Close)
	if err := server.configure(pluginConfig{Config: warmup.DefaultConfig(), CoreURL: core.URL, RecordUsage: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return core
}

func TestHistoryExecutionKeepsNativeTargetAndPublicCallback(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			native := &executionTestHost{}
			server := New(native)
			fakeHistoryCore(t, server)
			request := preparedExecution(provider)
			var response executionResponse
			if err := server.history.Call("host.model.execute", request, &response); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 200 || string(response.Body) != `{"result":"native response"}` || len(native.requests) != 1 {
				t.Fatalf("Response = %#v, executions = %d", response, len(native.requests))
			}
			actual := native.requests[0]
			if actual.AuthID != request.AuthID || actual.Model != request.Model || actual.ForcedProvider != provider || string(actual.Body) != string(request.Body) || actual.HostCallbackID != "public-api-callback" {
				t.Fatalf("Native request changed target or lost the public callback: %#v", actual)
			}
			if actual.Headers.Get("Authorization") != "" || actual.Headers.Get(executionHeader) != "" {
				t.Fatal("Local execution credentials reached the native request")
			}
			if len(server.history.tickets) != 0 || server.history.running != 0 {
				t.Fatal("Completed execution retained a ticket")
			}
		})
	}
}

func seedExecutionTicket(h *historyHost) (frontendAuthRequest, executorRequest) {
	token := executionPrefix + strings.Repeat("a", 64)
	body := []byte(`{"model":"` + executionModel + `","input":"Reply OK"}`)
	headers := http.Header{"Authorization": {"Bearer " + token}}
	headers.Set(executionHeader, token)
	h.enabled = true
	h.tickets[sha256.Sum256([]byte(token))] = &executionTicket{request: preparedExecution("codex"), path: "/v1/responses", body: body, expires: h.now().Add(ticketLifetime), done: make(chan struct{})}
	return frontendAuthRequest{Method: "POST", Path: "/v1/responses", Headers: headers, Body: body},
		executorRequest{SourceFormat: "openai-response", Model: executionModel, Headers: headers, OriginalRequest: body, Payload: body, Metadata: map[string]any{"request_path": "/v1/responses"}, HostCallbackID: "public-api-callback"}
}

func TestHistoryTicketCanAuthenticateAndExecuteOnlyOnce(t *testing.T) {
	native := &executionTestHost{}
	h := newHistoryHost(native)
	t.Cleanup(h.close)
	auth, execute := seedExecutionTicket(h)
	var accepted atomic.Int32
	var group sync.WaitGroup
	for range 20 {
		group.Go(func() {
			if h.authenticate(auth).Authenticated {
				accepted.Add(1)
			}
		})
	}
	group.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("Authentication accepted %d copies of one ticket", accepted.Load())
	}
	accepted.Store(0)
	for range 20 {
		group.Go(func() {
			if _, err := h.execute(execute); err == nil {
				accepted.Add(1)
			}
		})
	}
	group.Wait()
	if accepted.Load() != 1 || len(native.requests) != 1 {
		t.Fatalf("Duplicate executions: accepted=%d native=%d", accepted.Load(), len(native.requests))
	}
	if h.authenticate(auth).Authenticated {
		t.Fatal("A consumed ticket authenticated again")
	}
}

func TestHistoryAuthenticationRejectsModifiedRequests(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*frontendAuthRequest)
	}{
		{"method", func(r *frontendAuthRequest) { r.Method = "GET" }},
		{"path", func(r *frontendAuthRequest) { r.Path = "/v1/messages" }},
		{"query", func(r *frontendAuthRequest) { r.Query = url.Values{"route": {"other"}} }},
		{"body", func(r *frontendAuthRequest) { r.Body = []byte(`{"model":"gpt-6-astra"}`) }},
		{"oversized", func(r *frontendAuthRequest) { r.Body = []byte(strings.Repeat("x", maxRequestBody+1)) }},
		{"bearer", func(r *frontendAuthRequest) { r.Headers.Set("Authorization", "Bearer unrelated") }},
		{"duplicate header", func(r *frontendAuthRequest) { r.Headers.Add(executionHeader, "other") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHistoryHost(&executionTestHost{})
			t.Cleanup(h.close)
			auth, _ := seedExecutionTicket(h)
			test.change(&auth)
			if h.authenticate(auth).Authenticated {
				t.Fatal("An altered request authenticated")
			}
		})
	}
}

func TestHistoryExecutionRejectsInvalidTicketsBeforeNativeCall(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*historyHost, *executorRequest)
	}{
		{"missing callback", func(_ *historyHost, r *executorRequest) { r.HostCallbackID = "" }},
		{"changed body", func(_ *historyHost, r *executorRequest) { r.Payload = []byte(`{}`) }},
		{"changed original", func(_ *historyHost, r *executorRequest) { r.OriginalRequest = []byte(`{}`) }},
		{"changed model", func(_ *historyHost, r *executorRequest) { r.Model = "gpt-6-astra" }},
		{"changed protocol", func(_ *historyHost, r *executorRequest) { r.SourceFormat = "claude" }},
		{"changed path", func(_ *historyHost, r *executorRequest) { r.Metadata["request_path"] = "/v1/messages" }},
		{"stream", func(_ *historyHost, r *executorRequest) { r.Stream = true }},
		{"expired", func(h *historyHost, _ *executorRequest) {
			now := h.now().Add(ticketLifetime)
			h.now = func() time.Time { return now }
		}},
		{"closed", func(h *historyHost, _ *executorRequest) { h.close() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			native := &executionTestHost{}
			h := newHistoryHost(native)
			t.Cleanup(h.close)
			auth, execution := seedExecutionTicket(h)
			if !h.authenticate(auth).Authenticated {
				t.Fatal("Fixture did not authenticate")
			}
			test.change(h, &execution)
			if _, err := h.execute(execution); err == nil || len(native.requests) != 0 {
				t.Fatal("Invalid ticket reached native execution")
			}
		})
	}
}

func TestHistoryRoutesOnlyItsOwnNamespace(t *testing.T) {
	h := newHistoryHost(nil)
	t.Cleanup(h.close)
	if h.route(routeRequest{RequestedModel: "gpt-6-luna"}).Handled || h.authenticate(frontendAuthRequest{Method: "POST", Path: "/v1/responses"}).Authenticated {
		t.Fatal("Ordinary requests were changed")
	}
	if h.route(routeRequest{RequestedModel: "gpt-6-luna", Headers: http.Header{"Authorization": {"Bearer client-cpa-warmup.alice"}}}).Handled {
		t.Fatal("A native API key containing the ticket prefix was mistaken for a warmup")
	}
	for _, request := range []routeRequest{
		{RequestedModel: executionModel},
		{RequestedModel: "gpt-6-luna", Headers: http.Header{executionHeader: {"invalid"}}},
		{RequestedModel: "gpt-6-luna", Headers: http.Header{"Authorization": {"Bearer " + executionPrefix + "invalid"}}},
	} {
		if route := h.route(request); !route.Handled || route.TargetKind != "self" {
			t.Fatal("Invalid warmup request fell through to ordinary routing")
		}
	}
}

func TestHistoryCoreURLRequiresExplicitLiteralLoopbackOrigin(t *testing.T) {
	for _, input := range []string{"http://127.0.0.1:8317", "https://[::1]:9443/", "http://127.1.2.3:1234"} {
		if _, err := validateCoreURL(input); err != nil {
			t.Errorf("Valid loopback origin %q: %v", input, err)
		}
	}
	for _, input := range []string{"", "http://localhost:8317", "http://example.com:8317", "http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://user:pass@127.0.0.1:8317", "http://127.0.0.1:8317/path", "http://127.0.0.1:8317/?x=y", "http://127.0.0.1:8317/?", "http://127.0.0.1:8317/#fragment", "ftp://127.0.0.1:8317", "http://192.168.1.1:8317"} {
		if _, err := validateCoreURL(input); err == nil {
			t.Errorf("Accepted unsafe core origin %q", input)
		}
	}
}

func TestHistoryDoesNotTrustUnrelatedLoopbackServersOrFollowRedirects(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		var downstream atomic.Int32
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { downstream.Add(1) }))
		t.Cleanup(other.Close)
		core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if redirect {
				http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
				return
			}
			_, _ = w.Write([]byte(`{"result":"fake native response"}`))
		}))
		t.Cleanup(core.Close)
		server := New(&executionTestHost{})
		t.Cleanup(server.Close)
		if err := server.configure(pluginConfig{Config: warmup.DefaultConfig(), CoreURL: core.URL, RecordUsage: true}); err != nil {
			t.Fatal(err)
		}
		var response executionResponse
		err := server.history.Call("host.model.execute", preparedExecution("codex"), &response)
		if !redirect && err == nil {
			t.Fatal("Unrelated listener forged a successful execution")
		}
		if redirect && (err != nil || response.StatusCode != http.StatusTemporaryRedirect) {
			t.Fatalf("Redirect handling = %#v, %v", response, err)
		}
		if downstream.Load() != 0 || len(server.history.tickets) != 0 {
			t.Fatal("Redirect followed or unused ticket retained")
		}
	}
}

func TestHistoryNativeFailureAndPanicRemainFailures(t *testing.T) {
	for _, native := range []*executionTestHost{{err: executionError{429}}, {panic: true}} {
		server := New(native)
		fakeHistoryCore(t, server)
		var response executionResponse
		if err := server.history.Call("host.model.execute", preparedExecution("codex"), &response); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode < 400 || len(native.requests) != 1 || server.history.running != 0 {
			t.Fatalf("Failure escaped as success: %#v", response)
		}
		if strings.Contains(string(response.Body), "private") {
			t.Fatal("Native failure content leaked")
		}
	}
}

func TestHistoryCapabilitiesAreOptIn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		raw, _ := json.Marshal(registration(enabled))
		var metadata struct{ Capabilities map[string]json.RawMessage }
		_ = json.Unmarshal(raw, &metadata)
		for _, name := range []string{"frontend_auth_provider", "model_router", "executor"} {
			var declared bool
			if err := json.Unmarshal(metadata.Capabilities[name], &declared); err != nil || declared != enabled {
				t.Fatalf("Capability %s = %v, error %v", name, declared, err)
			}
		}
		if _, exclusive := metadata.Capabilities["frontend_auth_provider_exclusive"]; exclusive {
			t.Fatal("Warmup must not replace existing API authentication")
		}
	}
}

func TestHistoryCloseCancelsHTTPAndDrainsNativeExecution(t *testing.T) {
	native := &executionTestHost{entered: make(chan struct{}, 1), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(native.release) })
	server := New(native)
	fakeHistoryCore(t, server)
	callDone := make(chan error, 1)
	go func() {
		var response executionResponse
		callDone <- server.history.Call("host.model.execute", preparedExecution("codex"), &response)
	}()
	select {
	case <-native.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Native callback did not start")
	}
	closeDone := make(chan struct{})
	go func() { server.Close(); close(closeDone) }()
	select {
	case <-server.history.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not cancel the loopback request")
	}
	select {
	case <-callDone:
		t.Fatal("Canceled transport returned before the native callback")
	default:
	}
	select {
	case <-closeDone:
		t.Fatal("Shutdown did not wait for the native callback")
	default:
	}
	release.Do(func() { close(native.release) })
	select {
	case err := <-callDone:
		if err == nil {
			t.Fatal("Canceled transport returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Canceled transport did not finish after native execution")
	}
	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not finish after native execution")
	}
}

type executionEngineHost struct{ *executionTestHost }

func (h executionEngineHost) Call(method string, request, target any) error {
	if method == "host.auth.list" {
		return json.Unmarshal([]byte(`{"files":[{"id":"exact-account","provider":"codex","account_type":"oauth"}]}`), target)
	}
	return h.executionTestHost.Call(method, request, target)
}

type observedTransport struct {
	base   http.RoundTripper
	failed chan struct{}
	once   sync.Once
}

func (t *observedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		t.once.Do(func() { close(t.failed) })
	}
	return response, err
}

func (t *observedTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func TestHistoryHTTPTimeoutKeepsAccountGuardUntilNativeCompletion(t *testing.T) {
	native := &executionTestHost{entered: make(chan struct{}, 1), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(native.release) })
	server := New(executionEngineHost{native})
	fakeHistoryCore(t, server)
	transport := &observedTransport{base: server.history.client.Transport, failed: make(chan struct{})}
	server.history.client.Transport = transport
	server.history.client.Timeout = 50 * time.Millisecond
	firstDone := make(chan warmup.Result, 1)
	go func() {
		result, _ := server.engine.Warm(warmup.WarmRequest{Provider: "codex", AuthID: "exact-account", RequestID: "timeout-first"}, "")
		firstDone <- result
	}()
	select {
	case <-native.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Native callback did not start")
	}
	select {
	case <-transport.failed:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP timeout did not occur")
	}
	select {
	case <-firstDone:
		t.Fatal("HTTP timeout released the account while native execution was active")
	default:
	}
	second, err := server.engine.Warm(warmup.WarmRequest{Provider: "codex", AuthID: "exact-account", RequestID: "timeout-second"}, "")
	if err != nil || second.Status != "skipped" || !strings.Contains(second.Message, "in progress") {
		t.Fatalf("Overlapping warmup was not blocked: %#v, %v", second, err)
	}
	native.mu.Lock()
	count := len(native.requests)
	native.mu.Unlock()
	if count != 1 {
		t.Fatalf("Native executions = %d, want 1", count)
	}
	release.Do(func() { close(native.release) })
	select {
	case result := <-firstDone:
		if result.Status != "uncertain" {
			t.Fatalf("Lost HTTP response was reported as %s", result.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Warmup did not finish after the native callback")
	}
	state, err := server.engine.State("")
	if err != nil || len(state.Accounts) != 1 || !state.Accounts[0].Eligible {
		t.Fatalf("Completed account remained locked: %#v, %v", state, err)
	}
}

func TestHistoryHTTPTimeoutRevokesUnstartedTicket(t *testing.T) {
	native := &executionTestHost{}
	server := New(native)
	captured := make(chan executorRequest, 1)
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		auth := server.history.authenticate(frontendAuthRequest{Method: r.Method, Path: r.URL.Path, Headers: r.Header, Body: body})
		if !auth.Authenticated {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		captured <- executorRequest{SourceFormat: "openai-response", Model: executionModel, Headers: r.Header, OriginalRequest: body, Payload: body, Metadata: map[string]any{"request_path": r.URL.Path}, HostCallbackID: "public-api-callback"}
		<-r.Context().Done()
	}))
	t.Cleanup(core.Close)
	t.Cleanup(server.Close)
	if err := server.configure(pluginConfig{Config: warmup.DefaultConfig(), CoreURL: core.URL, RecordUsage: true}); err != nil {
		t.Fatal(err)
	}
	server.history.client.Timeout = 50 * time.Millisecond
	finished := make(chan error, 1)
	go func() {
		var response executionResponse
		finished <- server.history.Call("host.model.execute", preparedExecution("codex"), &response)
	}()
	var pending executorRequest
	select {
	case pending = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("Ticket did not reach authentication")
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("Timed-out request returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Unstarted request did not finish after timeout")
	}
	if _, err := server.history.execute(pending); err == nil || len(native.requests) != 0 {
		t.Fatal("Revoked ticket began native execution after its caller returned")
	}
}

func TestHistoryRejectsReconfigurationDuringExecution(t *testing.T) {
	server := New(&executionTestHost{})
	t.Cleanup(server.Close)
	seedExecutionTicket(server.history)
	before := server.history.url
	changed := pluginConfig{Config: warmup.DefaultConfig(), CoreURL: "http://127.0.0.1:1234", RecordUsage: false}
	if err := server.configure(changed); err == nil || server.history.url != before || !server.history.enabled {
		t.Fatal("A pending ticket's transport was changed")
	}
}

func TestHistoryTicketCapacityAndExpiry(t *testing.T) {
	server := New(&executionTestHost{})
	fakeHistoryCore(t, server)
	now := time.Now()
	server.history.now = func() time.Time { return now }
	for i := range maxTickets {
		var key [32]byte
		key[0] = byte(i)
		server.history.tickets[key] = &executionTicket{expires: now.Add(ticketLifetime)}
	}
	var response executionResponse
	if err := server.history.Call("host.model.execute", preparedExecution("codex"), &response); err == nil || len(server.history.tickets) != maxTickets {
		t.Fatal("A full ticket registry accepted another request")
	}
	now = now.Add(ticketLifetime)
	if err := server.history.Call("host.model.execute", preparedExecution("codex"), &response); err != nil || response.StatusCode != 200 || len(server.history.tickets) != 0 {
		t.Fatalf("Expired tickets were not reclaimed: %#v, %v", response, err)
	}
}
