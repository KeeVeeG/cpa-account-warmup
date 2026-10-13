package warmup

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testHost struct {
	mu         sync.Mutex
	entries    []authEntry
	listErr    error
	executeErr error
	response   modelResponse
	executions []modelRequest
	callbacks  []string
	started    chan struct{}
	release    chan struct{}
}

func newTestEngine(host Host, overrides Config) *Engine {
	engine := New(host)
	config := DefaultConfig()
	if overrides.ClaudeModel != "" {
		config.ClaudeModel = overrides.ClaudeModel
	}
	if overrides.CodexModel != "" {
		config.CodexModel = overrides.CodexModel
	}
	if overrides.CooldownSeconds != 0 {
		config.CooldownSeconds = overrides.CooldownSeconds
	}
	if err := engine.Configure(config); err != nil {
		panic(err)
	}
	return engine
}

func (h *testHost) Call(method string, request any, response any) error {
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	var callback struct {
		ID string `json:"host_callback_id"`
	}
	if err := json.Unmarshal(data, &callback); err != nil {
		return err
	}
	h.mu.Lock()
	h.callbacks = append(h.callbacks, callback.ID)
	switch method {
	case "host.auth.list":
		err := h.listErr
		entries := append([]authEntry(nil), h.entries...)
		h.mu.Unlock()
		if err != nil {
			return err
		}
		return copyJSON(response, authList{Files: entries})
	case "host.model.execute":
		var req modelRequest
		if err := json.Unmarshal(data, &req); err != nil {
			h.mu.Unlock()
			return err
		}
		h.executions = append(h.executions, req)
		err, result, started, release := h.executeErr, h.response, h.started, h.release
		h.mu.Unlock()
		if started != nil {
			started <- struct{}{}
		}
		if release != nil {
			<-release
		}
		if err != nil {
			return err
		}
		return copyJSON(response, result)
	default:
		h.mu.Unlock()
		return fmt.Errorf("unexpected host method: %s", method)
	}
}

func copyJSON(destination, source any) error {
	data, err := json.Marshal(source)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, destination)
}

func oauth(id, provider string) authEntry {
	return authEntry{ID: id, Provider: provider, Label: id, AccountType: "oauth", Status: "active"}
}

func successResponse(provider, model string) modelResponse {
	var body string
	if provider == "claude" {
		body = fmt.Sprintf(`{"type":"message","id":"msg_test","role":"assistant","model":%q,"stop_reason":"end_turn","content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":4,"output_tokens":1}}`, model)
	} else {
		body = fmt.Sprintf(`{"object":"response","id":"resp_test","status":"completed","model":%q,"output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":4,"output_tokens":1,"total_tokens":5}}`, model)
	}
	return modelResponse{StatusCode: 200, Body: []byte(body)}
}

func TestWarmPinsEachProviderWithoutCredentialSecrets(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			model := DefaultConfig().ClaudeModel
			if provider == "codex" {
				model = DefaultConfig().CodexModel
			}
			host := &testHost{entries: []authEntry{oauth("chosen-account", provider), oauth("another-account", provider)}, response: successResponse(provider, model)}
			engine := newTestEngine(host, Config{})
			result, err := engine.Warm(WarmRequest{Provider: provider, AuthID: "chosen-account", RequestID: "request-0001"}, "callback-123")
			if err != nil || result.Status != "success" {
				t.Fatalf("Warm = %#v, %v", result, err)
			}
			if len(host.executions) != 1 {
				t.Fatalf("executions = %d, want 1", len(host.executions))
			}
			payload, _ := json.Marshal(host.executions[0])
			var wire map[string]any
			if err := json.Unmarshal(payload, &wire); err != nil {
				t.Fatal(err)
			}
			for key, value := range map[string]string{"auth_id": "chosen-account", "forced_provider": provider, "model": model, "host_callback_id": "callback-123"} {
				if wire[key] != value {
					t.Fatalf("%s = %#v, want %q", key, wire[key], value)
				}
			}
			for _, id := range host.callbacks {
				if id != "callback-123" {
					t.Fatalf("callback ID = %q", id)
				}
			}
			if result.InputTokens != 4 || result.OutputTokens != 1 {
				t.Fatalf("token counts = %#v", result)
			}
			if !strings.Contains(result.Message, "not been verified") {
				t.Fatalf("success claims a verified quota window: %q", result.Message)
			}
		})
	}
}

func TestWarmReportsKnownUsageWithoutInventingMissingCounts(t *testing.T) {
	for _, reported := range []bool{false, true} {
		usage := `{"input_tokens":6,"output_tokens":1}`
		if reported {
			usage = `{"input_tokens":6,"output_tokens":1,"cache_read_input_tokens":100,"cache_creation_input_tokens":40,"output_tokens_details":{"thinking_tokens":0}}`
		}
		host := &testHost{
			entries:  []authEntry{oauth("chosen-account", "claude")},
			response: completionWithUsage(t, "claude", usage),
		}
		result, err := newTestEngine(host, Config{}).Warm(WarmRequest{"claude", "chosen-account", "request-0001"}, "")
		if err != nil || result.Status != "success" {
			t.Fatalf("Warm = %#v, %v", result, err)
		}
		if reported {
			if result.InputTokens != 146 || result.CacheReadTokens == nil || *result.CacheReadTokens != 100 || result.CacheCreationTokens == nil || *result.CacheCreationTokens != 40 || result.ReasoningTokens == nil || *result.ReasoningTokens != 0 {
				t.Fatalf("lost reported token breakdown: %#v", result)
			}
		} else if result.InputTokens != 6 || result.CacheReadTokens != nil || result.CacheCreationTokens != nil || result.ReasoningTokens != nil {
			t.Fatalf("inferred missing token breakdown: %#v", result)
		}
		raw, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"cache_read_tokens", "cache_creation_tokens", "reasoning_tokens"} {
			if _, present := fields[field]; present != reported {
				t.Fatalf("%s presence = %v, want %v: %s", field, present, reported, raw)
			}
		}
		if reported && string(fields["reasoning_tokens"]) != "0" {
			t.Fatalf("reported zero reasoning did not survive JSON: %s", raw)
		}
	}
}

func TestWarmRevalidatesAccountAndRefusesInvalidSelections(t *testing.T) {
	tests := []struct {
		name            string
		entries         []authEntry
		request         WarmRequest
		validationError bool
	}{
		{"wrong provider", []authEntry{oauth("account-a", "codex")}, WarmRequest{"claude", "account-a", "request-0001"}, false},
		{"missing", []authEntry{oauth("account-b", "claude")}, WarmRequest{"claude", "account-a", "request-0001"}, false},
		{"duplicate host ID", []authEntry{oauth("account-a", "claude"), oauth("account-a", "claude")}, WarmRequest{"claude", "account-a", "request-0001"}, false},
		{"api key", []authEntry{{ID: "account-a", Provider: "claude", AccountType: "api_key"}}, WarmRequest{"claude", "account-a", "request-0001"}, false},
		{"unknown auth type", []authEntry{{ID: "account-a", Provider: "claude"}}, WarmRequest{"claude", "account-a", "request-0001"}, false},
		{"disabled", []authEntry{{ID: "account-a", Provider: "claude", AccountType: "oauth", Disabled: true}}, WarmRequest{"claude", "account-a", "request-0001"}, false},
		{"unavailable", []authEntry{{ID: "account-a", Provider: "claude", AccountType: "oauth", Unavailable: true}}, WarmRequest{"claude", "account-a", "request-0001"}, false},
		{"unsupported provider", nil, WarmRequest{"openai", "account-a", "request-0001"}, true},
		{"control in ID", nil, WarmRequest{"claude", "account\na", "request-0001"}, true},
		{"empty ID", nil, WarmRequest{"claude", "", "request-0001"}, true},
		{"empty idempotency ID", nil, WarmRequest{"claude", "account-a", ""}, true},
		{"unsafe idempotency ID", nil, WarmRequest{"claude", "account-a", "request 0001"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := &testHost{entries: tt.entries}
			engine := newTestEngine(host, Config{})
			result, err := engine.Warm(tt.request, "")
			if (err != nil) != tt.validationError {
				t.Fatalf("error = %v", err)
			}
			if !tt.validationError && result.Status != "skipped" {
				t.Fatalf("status = %s", result.Status)
			}
			if len(host.executions) != 0 {
				t.Fatal("invalid selection reached model execution")
			}
		})
	}
}

func TestDuplicateRequestsAndAccountRacesExecuteOnlyOnce(t *testing.T) {
	host := &testHost{entries: []authEntry{oauth("account-a", "claude")}, response: successResponse("claude", DefaultConfig().ClaudeModel), started: make(chan struct{}, 1), release: make(chan struct{})}
	engine := newTestEngine(host, Config{})
	request := WarmRequest{"claude", "account-a", "request-0001"}
	finished := make(chan Result, 1)
	go func() { result, _ := engine.Warm(request, "callback-1"); finished <- result }()
	<-host.started
	var wg sync.WaitGroup
	var unexpected atomic.Int64
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			next := request
			if i%2 == 0 {
				next.RequestID = fmt.Sprintf("request-race-%d", i)
			}
			result, err := engine.Warm(next, "callback-race")
			if err != nil || result.Status != "skipped" {
				unexpected.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if unexpected.Load() != 0 {
		t.Fatalf("unexpected race results: %d", unexpected.Load())
	}
	close(host.release)
	if result := <-finished; result.Status != "success" {
		t.Fatalf("first result = %#v", result)
	}
	replay, err := engine.Warm(request, "another-callback")
	if err != nil || replay.Status != "success" || !replay.Duplicate {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	request.RequestID = "request-new-2"
	guarded, err := engine.Warm(request, "another-callback")
	if err != nil || guarded.Status != "skipped" || guarded.CooldownUntil == nil {
		t.Fatalf("guard = %#v, %v", guarded, err)
	}
	if len(host.executions) != 1 {
		t.Fatalf("executions = %d", len(host.executions))
	}
}

func TestRequestIDCannotBeReboundToAnotherAccount(t *testing.T) {
	host := &testHost{entries: []authEntry{oauth("account-a", "claude"), oauth("account-b", "claude")}, response: successResponse("claude", DefaultConfig().ClaudeModel)}
	engine := newTestEngine(host, Config{})
	_, err := engine.Warm(WarmRequest{"claude", "account-a", "request-0001"}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.Warm(WarmRequest{"claude", "account-b", "request-0001"}, "")
	if err == nil {
		t.Fatal("request ID accepted for another account")
	}
	if len(host.executions) != 1 {
		t.Fatal("rebound request executed")
	}
}

type secretError struct{ status int }

func (e secretError) Error() string   { return "Bearer secret-token PRIVATE upstream body" }
func (e secretError) StatusCode() int { return e.status }

func TestHostErrorsAreSanitizedAndNeverRetried(t *testing.T) {
	for _, code := range []int{0, 400, 401, 403, 404, 422, 429, 500, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			host := &testHost{entries: []authEntry{oauth("account-a", "claude")}, executeErr: secretError{code}}
			engine := newTestEngine(host, Config{})
			result, err := engine.Warm(WarmRequest{"claude", "account-a", "request-0001"}, "")
			if err != nil {
				t.Fatal(err)
			}
			wire, _ := json.Marshal(result)
			if strings.Contains(string(wire), "secret-token") || strings.Contains(string(wire), "PRIVATE") {
				t.Fatal("host error leaked")
			}
			if result.Status != "error" && result.Status != "uncertain" {
				t.Fatalf("status = %s", result.Status)
			}
			if len(host.executions) != 1 || result.CooldownUntil == nil {
				t.Fatalf("unsafe failure handling: %#v", result)
			}
		})
	}
}

func TestHostAccountListFailureSendsNoRequestAndRedactsError(t *testing.T) {
	host := &testHost{listErr: errors.New("secret-token")}
	engine := newTestEngine(host, Config{})
	_, err := engine.State("callback")
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("State error = %v", err)
	}
	result, err := engine.Warm(WarmRequest{"claude", "account-a", "request-0001"}, "callback")
	if err != nil || result.Status != "error" || result.CooldownUntil != nil || len(host.executions) != 0 {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
}

func TestNonSuccessResponseStatusUsesSafeClassification(t *testing.T) {
	for _, status := range []int{0, 400, 401, 403, 404, 422, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			host := &testHost{
				entries:  []authEntry{oauth("account-a", "claude")},
				response: modelResponse{StatusCode: status, Body: []byte(`{"error":"secret-token"}`)},
			}
			engine := newTestEngine(host, Config{})
			result, err := engine.Warm(WarmRequest{"claude", "account-a", "request-0001"}, "")
			if err != nil {
				t.Fatal(err)
			}
			want := "error"
			if status == 0 || status >= 500 {
				want = "uncertain"
			}
			if result.Status != want || result.CooldownUntil == nil {
				t.Fatalf("result = %#v, want %s", result, want)
			}
			if strings.Contains(result.Message, "secret-token") {
				t.Fatal("response body leaked")
			}
		})
	}
}

type panicHost struct {
	host   Host
	method string
}

func (h panicHost) Call(method string, request, response any) error {
	if method == h.method {
		panic("secret-token")
	}
	return h.host.Call(method, request, response)
}

func TestHostPanicDoesNotStrandAccountOrLeakPayload(t *testing.T) {
	for _, method := range []string{"host.auth.list", "host.model.execute"} {
		t.Run(method, func(t *testing.T) {
			host := &testHost{entries: []authEntry{oauth("account-a", "claude")}}
			engine := newTestEngine(panicHost{host: host, method: method}, Config{})
			request := WarmRequest{"claude", "account-a", "request-0001"}
			result, err := engine.Warm(request, "")
			if err != nil {
				t.Fatal(err)
			}
			want := "error"
			if method == "host.model.execute" {
				want = "uncertain"
			}
			if result.Status != want || strings.Contains(result.Message, "secret-token") {
				t.Fatalf("result = %#v", result)
			}
			if len(engine.active) != 0 || engine.requests[request.RequestID].running {
				t.Fatal("panic stranded an in-progress request")
			}
			if (result.CooldownUntil != nil) != (method == "host.model.execute") {
				t.Fatalf("incorrect attempt guard: %#v", result)
			}
			replay, err := engine.Warm(request, "")
			if err != nil || !replay.Duplicate || replay.Status != want {
				t.Fatalf("replay = %#v, %v", replay, err)
			}
		})
	}
}

func TestInvalidCompletionIsUncertainAndProtected(t *testing.T) {
	for _, body := range []string{`not JSON`, `{"error":{"message":"secret-token"}}`, `{"type":"message","model":"claude-opus-other","usage":{"input_tokens":1,"output_tokens":1}}`} {
		host := &testHost{entries: []authEntry{oauth("account-a", "claude")}, response: modelResponse{StatusCode: 200, Body: []byte(body)}}
		engine := newTestEngine(host, Config{})
		result, err := engine.Warm(WarmRequest{"claude", "account-a", "request-0001"}, "")
		if err != nil || result.Status != "uncertain" || result.CooldownUntil == nil {
			t.Fatalf("result = %#v, error = %v", result, err)
		}
		if strings.Contains(result.Message, "secret-token") {
			t.Fatal("response body leaked")
		}
	}
}

func TestGuardUsesLocalClockAndDoesNotInventProviderReset(t *testing.T) {
	host := &testHost{entries: []authEntry{oauth("account-a", "claude")}, response: successResponse("claude", DefaultConfig().ClaudeModel)}
	engine := newTestEngine(host, Config{CooldownSeconds: 30})
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	engine.now = func() time.Time { return now }
	result, err := engine.Warm(WarmRequest{"claude", "account-a", "request-0001"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.CooldownUntil.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("cooldown = %v", result.CooldownUntil)
	}
	state, err := engine.State("")
	if err != nil || state.Accounts[0].Eligible {
		t.Fatalf("guarded state = %#v, %v", state, err)
	}
	now = now.Add(31 * time.Second)
	state, err = engine.State("")
	if err != nil || !state.Accounts[0].Eligible || state.Accounts[0].CooldownUntil != nil {
		t.Fatalf("expired state = %#v, %v", state, err)
	}
	result, err = engine.Warm(WarmRequest{"claude", "account-a", "request-0002"}, "")
	if err != nil || result.Status != "success" || len(host.executions) != 2 {
		t.Fatalf("second = %#v, %v", result, err)
	}
}

func TestStateReturnsOnlySupportedProvidersAndSafeMetadata(t *testing.T) {
	host := &testHost{entries: []authEntry{oauth("b", "codex"), oauth("a", "claude"), oauth("other", "gemini"), {ID: "api", Provider: "claude", AccountType: "api_key", Label: strings.Repeat("x", 200) + "\n"}}}
	state, err := newTestEngine(host, Config{}).State("")
	if err != nil || len(state.Accounts) != 3 {
		t.Fatalf("state = %#v, %v", state, err)
	}
	for _, account := range state.Accounts {
		if len([]rune(account.Label)) > 160 || strings.Contains(account.Label, "\n") {
			t.Fatalf("unbounded label: %#v", account)
		}
		if account.ID == "api" && account.Eligible {
			t.Fatal("API key is eligible")
		}
	}
}

func TestElapsedHostCooldownAllowsPinnedWarmup(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name       string
		retryAfter time.Time
		disabled   bool
		eligible   bool
	}{
		{"future", now.Add(time.Minute), false, false},
		{"missing recovery time", time.Time{}, false, false},
		{"elapsed", now.Add(-time.Minute), false, true},
		{"exactly elapsed", now, false, true},
		{"disabled remains disabled", now.Add(-time.Minute), true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			entry := oauth("account-a", "claude")
			entry.Unavailable, entry.Disabled, entry.NextRetryAfter = true, tt.disabled, tt.retryAfter
			host := &testHost{entries: []authEntry{entry}, response: successResponse("claude", DefaultConfig().ClaudeModel)}
			engine := newTestEngine(host, Config{})
			engine.now = func() time.Time { return now }
			state, err := engine.State("")
			if err != nil || len(state.Accounts) != 1 || state.Accounts[0].Eligible != tt.eligible {
				t.Fatalf("state = %#v, %v", state, err)
			}
			result, err := engine.Warm(WarmRequest{"claude", "account-a", "request-0001"}, "")
			if err != nil {
				t.Fatal(err)
			}
			if tt.eligible {
				if result.Status != "success" || len(host.executions) != 1 {
					t.Fatalf("eligible result = %#v", result)
				}
			} else if result.Status != "skipped" || len(host.executions) != 0 {
				t.Fatalf("blocked result = %#v", result)
			}
		})
	}
}

func TestConfigureIsAtomicAndRejectsRoutingExpressions(t *testing.T) {
	engine := newTestEngine(nil, Config{})
	mutations := []func(*Config){
		func(c *Config) { c.ClaudeModel = "auto" },
		func(c *Config) { c.ClaudeModel = "" },
		func(c *Config) { c.CodexModel = "" },
		func(c *Config) { c.ClaudeModel = "claude-test(high)" },
		func(c *Config) { c.ClaudeModel = "provider/claude-test" },
		func(c *Config) { c.CodexModel = "gpt-test\n" },
		func(c *Config) { c.CooldownSeconds = -1 },
		func(c *Config) { c.CooldownSeconds = 0 },
		func(c *Config) { c.CooldownSeconds = 86401 },
	}
	for _, mutate := range mutations {
		config := DefaultConfig()
		mutate(&config)
		if err := engine.Configure(config); err == nil {
			t.Fatalf("accepted config %#v", config)
		}
		if engine.config != DefaultConfig() {
			t.Fatalf("partially applied config: %#v", engine.config)
		}
	}
}

func TestHistoryAndRequestCacheAreBounded(t *testing.T) {
	host := &testHost{entries: nil}
	engine := newTestEngine(host, Config{})
	for i := 0; i < maxRequestCache+20; i++ {
		_, err := engine.Warm(WarmRequest{"claude", "missing", fmt.Sprintf("request-%08d", i)}, "")
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(engine.requests) > maxRequestCache || len(engine.recent) > maxRecentResults {
		t.Fatalf("unbounded cache: requests=%d recent=%d", len(engine.requests), len(engine.recent))
	}
}
