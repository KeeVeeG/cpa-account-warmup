package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type testHost struct{ calls int }

func (h *testHost) Call(method string, request any, response any) error {
	h.calls++
	return json.Unmarshal([]byte(`{"files":[]}`), response)
}

func TestRegisterDeclaresOnlyAuthenticatedActions(t *testing.T) {
	server := New(&testHost{})
	result, err := server.dispatch("management.register", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var registration struct {
		Routes    []struct{ Method, Path string }
		Resources []struct{ Path, Menu string }
	}
	if err := json.Unmarshal(raw, &registration); err != nil {
		t.Fatal(err)
	}
	if len(registration.Routes) != 2 {
		t.Fatalf("Expected two routes: %s", raw)
	}
	for _, route := range registration.Routes {
		if !strings.HasPrefix(route.Path, "/plugins/"+ID+"/") {
			t.Fatalf("Unexpected action path: %s", route.Path)
		}
	}
	for _, resource := range registration.Resources {
		if strings.Contains(resource.Path, "warmup") {
			t.Fatal("Public resources must not perform warmups")
		}
	}
}

func TestPublicResourcesNeverCallHost(t *testing.T) {
	host := &testHost{}
	server := New(host)
	for _, path := range []string{"/status", "/app.js", "/styles.css"} {
		response := server.management(managementRequest{Method: "GET", Path: ResourceBase + path})
		if response.StatusCode != 200 || len(response.Body) == 0 {
			t.Fatalf("Asset %s was not served", path)
		}
		if response.Headers.Get("Content-Security-Policy") == "" {
			t.Fatal("Missing content policy")
		}
		if response.Headers.Get("Cache-Control") != "no-store" {
			t.Fatal("Resource must not be cached")
		}
	}
	if host.calls != 0 {
		t.Fatal("Public asset requests invoked host callbacks")
	}
}

func TestManagementGuardsRejectBeforeHostCall(t *testing.T) {
	cases := []struct {
		name     string
		method   string
		path     string
		headers  http.Header
		callback string
		body     string
		status   int
	}{
		{"no key", "GET", APIBase + "/state", nil, "callback", "", 401},
		{"empty bearer", "GET", APIBase + "/state", http.Header{"Authorization": {"Bearer "}}, "callback", "", 401},
		{"missing callback", "GET", APIBase + "/state", http.Header{"Authorization": {"Bearer test-only"}}, "", "", 503},
		{"public warmup", "POST", ResourceBase + "/warmup", nil, "callback", "{}", 404},
		{"wrong method", "GET", APIBase + "/warmup", http.Header{"Authorization": {"Bearer test-only"}}, "callback", "", 405},
		{"wrong media type", "POST", APIBase + "/warmup", http.Header{"Authorization": {"Bearer test-only"}, "Content-Type": {"text/plain"}}, "callback", "{}", 415},
		{"cross site", "POST", APIBase + "/warmup", http.Header{"Authorization": {"Bearer test-only"}, "Content-Type": {"application/json"}, "Sec-Fetch-Site": {"cross-site"}}, "callback", "{}", 403},
		{"extra fields", "POST", APIBase + "/warmup", http.Header{"Authorization": {"Bearer test-only"}, "Content-Type": {"application/json"}}, "callback", `{"provider":"claude","secret":"no"}`, 400},
		{"trailing object", "POST", APIBase + "/warmup", http.Header{"Authorization": {"Bearer test-only"}, "Content-Type": {"application/json"}}, "callback", `{} {}`, 400},
		{"oversized", "POST", APIBase + "/warmup", http.Header{"Authorization": {"Bearer test-only"}, "Content-Type": {"application/json"}}, "callback", strings.Repeat("x", 8193), 413},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			host := &testHost{}
			response := New(host).management(managementRequest{Method: test.method, Path: test.path, Headers: test.headers, HostCallbackID: test.callback, Body: []byte(test.body)})
			if response.StatusCode != test.status {
				t.Fatalf("Got status %d, want %d", response.StatusCode, test.status)
			}
			if host.calls != 0 {
				t.Fatal("Rejected request called the host")
			}
		})
	}
}

func TestStateWithValidContext(t *testing.T) {
	host := &testHost{}
	response := New(host).management(managementRequest{Method: "GET", Path: APIBase + "/state", Headers: http.Header{"Authorization": {"Bearer test-only"}}, HostCallbackID: "test-callback"})
	if response.StatusCode != 200 || host.calls != 1 {
		t.Fatalf("State read failed: %+v", response)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatal(err)
	}
	if string(body["accounts"]) != "[]" {
		t.Fatalf("Expected an empty array, got %s", response.Body)
	}
}

func TestConfigurationRejectsMalformedAndUnknownFields(t *testing.T) {
	for _, input := range []string{"[not, a, mapping]", "claude_modle: typo", "cooldown_seconds: false", "enabled: true\n---\ncodex_model: gpt-other"} {
		if _, err := parseConfig([]byte(input)); err == nil {
			t.Errorf("Accepted invalid configuration: %q", input)
		}
	}
	config, err := parseConfig([]byte("enabled: true\npriority: 1\nstore:\n  version: 0.1.0\nclaude_model: claude-haiku-5-5\ncodex_model: gpt-6-luna\ncooldown_seconds: 120"))
	if err != nil {
		t.Fatal(err)
	}
	if config.CooldownSeconds != 120 || config.CodexModel != "gpt-6-luna" {
		t.Fatalf("Configuration was not preserved: %+v", config)
	}
}

func TestRPCNeverIncludesUnknownMethodOrRawInput(t *testing.T) {
	response := New(&testHost{}).Handle("secret-method-name", []byte(`{"secret":"never-render"}`))
	if strings.Contains(string(response), "secret-method-name") || strings.Contains(string(response), "never-render") {
		t.Fatal("Raw request reflected in error")
	}
	var envelope Envelope
	if err := json.Unmarshal(response, &envelope); err != nil || envelope.OK || envelope.Error == nil {
		t.Fatalf("Invalid error envelope: %s", response)
	}
}
