package plugin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cliproxy-account-warmup/internal/warmup"
)

func registeredServer(t *testing.T, host warmup.Host) *Server {
	t.Helper()
	server := New(host)
	t.Cleanup(server.Close)
	if _, err := server.dispatch("plugin.register", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	return server
}

func sessionToken(t *testing.T, server *Server) string {
	t.Helper()
	response := server.management(managementRequest{
		Method: http.MethodGet, Path: ResourceBase + server.local.launchPath,
	})
	if response.StatusCode != http.StatusFound {
		t.Fatalf("Launch returned %d", response.StatusCode)
	}
	destination, err := url.Parse(response.Headers.Get("Location"))
	if err != nil || destination.Scheme+"://"+destination.Host != server.local.origin || destination.Path != "/" {
		t.Fatal("Launch did not redirect to the local interface")
	}
	values, err := url.ParseQuery(destination.Fragment)
	if err != nil || len(values.Get("session")) != 64 {
		t.Fatal("Launch did not return a session fragment")
	}
	if destination.RawQuery != "" || response.Headers.Get("Referrer-Policy") != "no-referrer" || response.Headers.Get("Cache-Control") != "no-store" {
		t.Fatal("Launch must not expose the session in a query or cached response")
	}
	return values.Get("session")
}

func localRequest(server *Server, method, path, token, body string) *http.Request {
	request := httptest.NewRequest(method, server.local.origin+path, strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("Origin", server.local.origin)
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestLocalServerStartsOnlyAfterRegistration(t *testing.T) {
	server := New(&testHost{})
	t.Cleanup(server.Close)
	if _, err := server.dispatch("management.register", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if server.local != nil {
		t.Fatal("Constructing or inspecting a plugin started a listener")
	}
	if _, err := server.dispatch("plugin.register", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	local := server.local
	if local == nil || !strings.HasPrefix(local.address, "127.0.0.1:") {
		t.Fatal("Plugin registration did not bind a loopback listener")
	}
	if _, err := server.dispatch("plugin.reconfigure", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if server.local != local {
		t.Fatal("Reconfiguration replaced the listener and invalidated open tabs")
	}
	server.Close()
	if err := server.startLocal(); err == nil {
		t.Fatal("A closed plugin started another listener")
	}
}

func TestLaunchCapabilityDoesNotReadOrWarmAccounts(t *testing.T) {
	host := &testHost{}
	server := registeredServer(t, host)
	registration, err := server.dispatch("management.register", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}
	var data struct{ Resources []struct{ Path, Menu string } }
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	menus := 0
	for _, resource := range data.Resources {
		if resource.Menu == "" {
			continue
		}
		menus++
		if resource.Path != server.local.launchPath || len(strings.TrimPrefix(resource.Path, "/launch/")) != 64 {
			t.Fatal("Menu does not use an unguessable launch path")
		}
	}
	if menus != 1 {
		t.Fatalf("Got %d menus, want one", menus)
	}
	first, second := sessionToken(t, server), sessionToken(t, server)
	if first == second {
		t.Fatal("Opening another tab reused its session")
	}
	for _, path := range []string{"/status", "/app.js", "/styles.css", "/launch/wrong"} {
		response := server.management(managementRequest{Method: "GET", Path: ResourceBase + path})
		if strings.Contains(string(response.Body), first) || strings.Contains(string(response.Body), server.local.launchPath) || response.Headers.Get("Location") != "" {
			t.Fatal("A stable public resource disclosed a capability")
		}
	}
	if host.calls != 0 {
		t.Fatal("Opening the interface invoked the host")
	}
}

func TestLocalAPIRejectsUntrustedRequestsBeforeHostCalls(t *testing.T) {
	host := &testHost{}
	server := registeredServer(t, host)
	token := sessionToken(t, server)
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		change func(*http.Request)
		status int
	}{
		{"missing token", "GET", "/api/state", "", func(r *http.Request) { r.Header.Del("Authorization") }, 401},
		{"wrong token", "GET", "/api/state", "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+strings.Repeat("0", 64)) }, 401},
		{"query token", "GET", "/api/state?session=" + token, "", func(r *http.Request) { r.Header.Del("Authorization") }, 401},
		{"cookie token", "GET", "/api/state", "", func(r *http.Request) {
			r.Header.Del("Authorization")
			r.AddCookie(&http.Cookie{Name: "session", Value: token})
		}, 401},
		{"DNS rebinding", "GET", "/api/state", "", func(r *http.Request) { r.Host = "attacker.example" }, 403},
		{"foreign origin", "GET", "/api/state", "", func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") }, 403},
		{"opaque origin", "GET", "/api/state", "", func(r *http.Request) { r.Header.Set("Origin", "null") }, 403},
		{"cross-site", "GET", "/api/state", "", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
		{"preflight", "OPTIONS", "/api/warmup", "", nil, 405},
		{"warmup GET", "GET", "/api/warmup", "", nil, 405},
		{"state POST", "POST", "/api/state", "", nil, 405},
		{"unknown route", "POST", "/api/execute", "{}", nil, 404},
		{"wrong content type", "POST", "/api/warmup", "{}", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"oversized", "POST", "/api/warmup", strings.Repeat("x", maxRequestBody+1), nil, 413},
		{"unknown field", "POST", "/api/warmup", `{"provider":"claude","arbitrary_prompt":"test"}`, nil, 400},
		{"trailing JSON", "POST", "/api/warmup", `{} {}`, nil, 400},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := localRequest(server, test.method, test.path, token, test.body)
			if test.change != nil {
				test.change(request)
			}
			response := server.local.handle(request)
			if response.StatusCode != test.status {
				t.Fatalf("Got %d, want %d", response.StatusCode, test.status)
			}
			if response.Headers.Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("Local interface enabled CORS")
			}
			if host.calls != 0 {
				t.Fatal("Rejected request invoked the host")
			}
		})
	}
}

func TestSessionsExpireAndStayBounded(t *testing.T) {
	server := registeredServer(t, &testHost{})
	now := time.Now()
	server.local.now = func() time.Time { return now }
	first := sessionToken(t, server)
	for range maxSessions {
		now = now.Add(time.Second)
		_ = sessionToken(t, server)
	}
	if len(server.local.sessions) != maxSessions {
		t.Fatal("Session storage exceeded its limit")
	}
	request := localRequest(server, "GET", "/api/state", first, "")
	if server.local.authorized(request.Header) {
		t.Fatal("Oldest session was not evicted")
	}
	current := sessionToken(t, server)
	request.Header.Set("Authorization", "Bearer "+current)
	if !server.local.authorized(request.Header) {
		t.Fatal("Fresh session was rejected")
	}
	now = now.Add(sessionLifetime)
	if server.local.authorized(request.Header) {
		t.Fatal("Expired session was accepted")
	}
	server.Close()
	if len(server.local.sessions) != 0 || server.local.launch().StatusCode != 503 {
		t.Fatal("Shutdown did not invalidate sessions and launch capability")
	}
}

type localTestHost struct {
	calls      atomic.Int32
	modelCalls atomic.Int32
}

func (h *localTestHost) Call(method string, request, target any) error {
	h.calls.Add(1)
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	var input struct {
		CallbackID string `json:"host_callback_id"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	if input.CallbackID != "" {
		return io.ErrUnexpectedEOF
	}
	if method == "host.auth.list" {
		return json.Unmarshal([]byte(`{"files":[{"id":"claude-test","provider":"claude","account_type":"oauth","label":"Test account"}]}`), target)
	}
	h.modelCalls.Add(1)
	body := []byte(`{"id":"msg_test","type":"message","role":"assistant","model":"claude-haiku-5-5","stop_reason":"end_turn","content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":2,"output_tokens":1}}`)
	raw, err = json.Marshal(map[string]any{"status_code": 200, "body": body})
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func TestLocalWarmupUsesBackgroundHostCallback(t *testing.T) {
	host := &localTestHost{}
	server := registeredServer(t, host)
	token := sessionToken(t, server)
	request := localRequest(server, "POST", "/api/warmup", token,
		`{"provider":"claude","auth_id":"claude-test","request_id":"local-warmup-test"}`)
	response := server.local.handle(request)
	var result warmup.Result
	if err := json.Unmarshal(response.Body, &result); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || result.Status != "success" || host.calls.Load() != 2 || host.modelCalls.Load() != 1 {
		t.Fatalf("Local warmup did not complete through the host: %s", response.Body)
	}
}

func TestLoopbackListenerServesAuthenticatedStateWithoutManagementKey(t *testing.T) {
	host := &localTestHost{}
	server := registeredServer(t, host)
	token := sessionToken(t, server)
	request, err := http.NewRequest("GET", server.local.origin+"/api/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || host.calls.Load() != 1 {
		t.Fatal("Local session could not read state through the native host")
	}
}

type blockingLocalHost struct {
	entered chan struct{}
	release chan struct{}
}

func (h *blockingLocalHost) Call(string, any, any) error {
	close(h.entered)
	<-h.release
	return nil
}

func TestCloseDrainsHostCallbacks(t *testing.T) {
	host := &blockingLocalHost{entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(host.release) })
	server := registeredServer(t, host)
	token := sessionToken(t, server)
	request, err := http.NewRequest("GET", server.local.origin+"/api/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
	}()
	select {
	case <-host.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Host callback did not start")
	}
	shutdownStarted, closeDone := make(chan struct{}), make(chan struct{})
	server.local.http.RegisterOnShutdown(func() { close(shutdownStarted) })
	go func() { server.Close(); close(closeDone) }()
	select {
	case <-shutdownStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not start")
	}
	select {
	case <-closeDone:
		t.Fatal("Shutdown returned while a host callback was active")
	default:
	}
	release.Do(func() { close(host.release) })
	for _, done := range []chan struct{}{requestDone, closeDone} {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Shutdown did not finish after the host callback returned")
		}
	}
}
