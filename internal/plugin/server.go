// Package plugin implements the CLIProxyAPI JSON plugin contract.
package plugin

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"cpa-account-warmup/internal/warmup"
	"gopkg.in/yaml.v3"
)

const ID = "cpa-account-warmup"
const Version = "0.4.0"
const APIBase = "/v0/management/plugins/" + ID
const ResourceBase = "/v0/resource/plugins/" + ID

// Release builds can override the repository URL for forks.
var RepositoryURL = "https://github.com/KeeVeeG/" + ID

// CLIProxyAPI v8.0.27 still registers custom plugin routes under /v0/management.
// Its built-in configuration routes use /v8/management instead.

//go:embed web/*
var assets embed.FS

type Server struct {
	engine    *warmup.Engine
	history   *historyHost
	mu        sync.Mutex
	local     *localServer
	closed    bool
	closeOnce sync.Once
}

func New(host warmup.Host) *Server {
	history := newHistoryHost(host)
	return &Server{engine: warmup.New(history), history: history}
}

type Envelope struct {
	OK     bool      `json:"ok"`
	Result any       `json:"result,omitempty"`
	Error  *RPCError `json:"error,omitempty"`
}

type RPCError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type managementRequest struct {
	Method         string
	Path           string
	Headers        http.Header
	Query          url.Values
	Body           []byte
	HostCallbackID string `json:"host_callback_id"`
}

type managementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

func (s *Server) Handle(method string, raw []byte) []byte {
	result, err := s.dispatch(method, raw)
	if err != nil {
		var status interface{ StatusCode() int }
		if errors.As(err, &status) {
			return ErrorEnvelope("plugin_error", err.Error(), status.StatusCode())
		}
		return ErrorEnvelope("plugin_error", err.Error())
	}
	b, err := json.Marshal(Envelope{OK: true, Result: result})
	if err != nil {
		return ErrorEnvelope("serialization_error", "Could not encode the plugin response.")
	}
	return b
}

func ErrorEnvelope(code, message string, statuses ...int) []byte {
	failure := &RPCError{Code: code, Message: message}
	if len(statuses) == 1 && statuses[0] >= 400 && statuses[0] <= 599 {
		failure.HTTPStatus = statuses[0]
	}
	b, _ := json.Marshal(Envelope{Error: failure})
	return b
}

func (s *Server) dispatch(method string, raw []byte) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, errors.New("Invalid plugin configuration envelope.")
		}
		cfg, err := parseConfig(req.ConfigYAML)
		if err != nil {
			return nil, err
		}
		if err := s.configure(cfg); err != nil {
			return nil, err
		}
		if method == "plugin.register" {
			if err := s.startLocal(); err != nil {
				return nil, errors.New("Could not start the Account Warmup interface.")
			}
		}
		return registration(cfg.RecordUsage), nil
	case "frontend_auth.identifier", "executor.identifier":
		return map[string]string{"identifier": ID}, nil
	case "frontend_auth.authenticate":
		var req frontendAuthRequest
		if json.Unmarshal(raw, &req) != nil {
			return frontendAuthResponse{}, nil
		}
		return s.history.authenticate(req), nil
	case "model.route":
		var req routeRequest
		if json.Unmarshal(raw, &req) != nil {
			return routeResponse{}, nil
		}
		return s.history.route(req), nil
	case "executor.execute":
		var req executorRequest
		if json.Unmarshal(raw, &req) != nil {
			return nil, executionError{http.StatusBadRequest}
		}
		return s.history.execute(req)
	case "executor.execute_stream", "executor.count_tokens", "executor.http_request":
		return nil, executionError{http.StatusBadRequest}
	case "plugin.shutdown":
		s.Close()
		return struct{}{}, nil
	case "management.register":
		resources := []map[string]string{
			{"Path": "/status"},
			{"Path": "/bootstrap.js"},
			{"Path": "/app.js"},
			{"Path": "/styles.css"},
			{"Path": "/claude.svg"},
			{"Path": "/codex.svg"},
		}
		s.mu.Lock()
		if s.local != nil && !s.closed {
			resources = append(resources, map[string]string{
				"Path": s.local.launchPath, "Menu": "Account Warmup",
				"Description": "Warm up selected Claude or Codex accounts.",
			})
		}
		s.mu.Unlock()
		return map[string]any{
			"routes": []map[string]string{
				{"Method": "GET", "Path": "/plugins/" + ID + "/state"},
				{"Method": "POST", "Path": "/plugins/" + ID + "/warmup"},
			},
			"resources": resources,
		}, nil
	case "management.handle":
		var req managementRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, errors.New("Invalid management request.")
		}
		return s.management(req), nil
	default:
		return nil, errors.New("Unsupported plugin method.")
	}
}

type pluginConfig struct {
	warmup.Config
	CoreURL     string
	RecordUsage bool
}

func (s *Server) configure(cfg pluginConfig) error {
	s.history.mu.Lock()
	defer s.history.mu.Unlock()
	if s.history.closed {
		return errors.New("Account Warmup is closed")
	}
	if (s.history.enabled != cfg.RecordUsage || s.history.url != cfg.CoreURL) && (len(s.history.tickets) != 0 || s.history.running != 0) {
		return errors.New("Wait for the current warmup to finish before changing Usage settings")
	}
	if err := s.engine.Configure(cfg.Config); err != nil {
		return err
	}
	s.history.url, s.history.enabled = cfg.CoreURL, cfg.RecordUsage
	return nil
}

func parseConfig(raw []byte) (pluginConfig, error) {
	cfg := pluginConfig{Config: warmup.DefaultConfig(), CoreURL: defaultCoreURL}
	// A node map preserves explicit zero values so invalid settings cannot silently
	// turn into defaults. Host-owned fields are allowed but never interpreted here.
	var fields map[string]yaml.Node
	if len(bytes.TrimSpace(raw)) == 0 {
		return cfg, nil
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&fields); err != nil {
		return cfg, errors.New("Plugin configuration must be a YAML mapping.")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return cfg, errors.New("Only one configuration document is supported.")
	}
	for key, node := range fields {
		var err error
		switch key {
		case "claude_model":
			err = node.Decode(&cfg.ClaudeModel)
		case "codex_model":
			err = node.Decode(&cfg.CodexModel)
		case "cooldown_seconds":
			// Accepted for existing installations; completed warmups have no cooldown.
			var ignored int
			err = node.Decode(&ignored)
		case "core_url":
			err = node.Decode(&cfg.CoreURL)
		case "record_usage":
			err = node.Decode(&cfg.RecordUsage)
		case "enabled", "priority", "store":
			continue
		default:
			return cfg, errors.New("Unknown plugin configuration field.")
		}
		if err != nil {
			return cfg, errors.New("Invalid plugin configuration field type.")
		}
	}
	var err error
	cfg.CoreURL, err = validateCoreURL(cfg.CoreURL)
	return cfg, err
}

func registration(recordUsage bool) any {
	return map[string]any{
		"schema_version": 6,
		"metadata": map[string]any{
			"Name": "Account Warmup", "Version": Version,
			"Author": "KeeVeeG", "GitHubRepository": RepositoryURL,
			"ConfigFields": []map[string]string{
				{"Name": "claude_model", "Type": "string", "Description": "Exact Claude model ID. Default: claude-haiku-5-5. No automatic fallback."},
				{"Name": "codex_model", "Type": "string", "Description": "Exact Codex model ID. Default: gpt-6-luna. No automatic fallback."},
				{"Name": "record_usage", "Type": "boolean", "Description": "Add warmup requests to Usage history. Enable only when the core requires an API Access key. Default: false."},
				{"Name": "core_url", "Type": "string", "Description": "Core URL for Usage history. Default: http://127.0.0.1:8317. Loopback IP addresses only."},
			},
		},
		"capabilities": map[string]any{
			"management_api":          true,
			"frontend_auth_provider":  recordUsage,
			"model_router":            recordUsage,
			"executor":                recordUsage,
			"executor_input_formats":  []string{"claude", "openai-response"},
			"executor_output_formats": []string{"claude", "openai-response"},
		},
	}
}

func (s *Server) management(req managementRequest) managementResponse {
	if req.Method == "GET" {
		s.mu.Lock()
		local := s.local
		s.mu.Unlock()
		if local != nil && req.Path == ResourceBase+local.launchPath {
			return assetResponse("bootstrap.html", "text/html; charset=utf-8")
		}
		var name, contentType string
		switch req.Path {
		case ResourceBase + "/status":
			name, contentType = "index.html", "text/html; charset=utf-8"
		case ResourceBase + "/app.js":
			name, contentType = "app.js", "text/javascript; charset=utf-8"
		case ResourceBase + "/bootstrap.js":
			name, contentType = "bootstrap.js", "text/javascript; charset=utf-8"
		case ResourceBase + "/styles.css":
			name, contentType = "styles.css", "text/css; charset=utf-8"
		case ResourceBase + "/claude.svg", ResourceBase + "/codex.svg":
			name, contentType = strings.TrimPrefix(req.Path, ResourceBase+"/"), "image/svg+xml"
		}
		if name != "" {
			return assetResponse(name, contentType)
		}
	}
	if req.Path != APIBase+"/state" && req.Path != APIBase+"/warmup" {
		return jsonResponse(404, map[string]string{"error": "Route not found."})
	}
	// The host authenticates these routes. This additional check rejects accidental
	// exposure if a future integration dispatches them as public resources.
	if !hasManagementKey(req.Headers) {
		return jsonResponse(401, map[string]string{"error": "Sign in with the management key."})
	}
	if req.HostCallbackID == "" {
		return jsonResponse(503, map[string]string{"error": "The host did not provide a callback context. CLIProxyAPI 8.0.27 or newer is required."})
	}
	return s.action(req)
}

func (s *Server) action(req managementRequest) managementResponse {
	if req.Path == APIBase+"/state" && req.Method == "GET" {
		state, err := s.engine.State(req.HostCallbackID)
		if err != nil {
			return jsonResponse(502, map[string]string{"error": "Could not read accounts from CLIProxyAPI."})
		}
		return jsonResponse(200, state)
	}
	if req.Path == APIBase+"/warmup" && req.Method == "POST" {
		mediaType, _, err := mime.ParseMediaType(req.Headers.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			return jsonResponse(415, map[string]string{"error": "Send application/json."})
		}
		if len(req.Body) > maxRequestBody {
			return jsonResponse(413, map[string]string{"error": "Request body is too large."})
		}
		if req.Headers.Get("Sec-Fetch-Site") == "cross-site" {
			return jsonResponse(403, map[string]string{"error": "Cross-site warmup requests are not accepted."})
		}
		var input warmup.WarmRequest
		decoder := json.NewDecoder(bytes.NewReader(req.Body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			return jsonResponse(400, map[string]string{"error": "Invalid warmup request."})
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return jsonResponse(400, map[string]string{"error": "Send one JSON object."})
		}
		result, err := s.engine.Warm(input, req.HostCallbackID)
		if err != nil {
			return jsonResponse(400, map[string]string{"error": "The request could not be started. Refresh the account list and verify the selected provider and account."})
		}
		return jsonResponse(200, result)
	}
	return jsonResponse(405, map[string]string{"error": "Method not allowed."})
}

func hasManagementKey(headers http.Header) bool {
	auth := strings.TrimSpace(headers.Get("Authorization"))
	return (strings.HasPrefix(strings.ToLower(auth), "bearer ") && strings.TrimSpace(auth[7:]) != "") || strings.TrimSpace(headers.Get("X-Management-Key")) != ""
}

func responseHeaders(contentType string) http.Header {
	return http.Header{
		"Content-Type":            {contentType},
		"Cache-Control":           {"no-store"},
		"X-Content-Type-Options":  {"nosniff"},
		"Referrer-Policy":         {"no-referrer"},
		"Content-Security-Policy": {"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'"},
	}
}

func jsonResponse(status int, body any) managementResponse {
	raw, err := json.Marshal(body)
	if err != nil {
		status, raw = 500, []byte(`{"error":"Response encoding failed."}`)
	}
	return managementResponse{StatusCode: status, Headers: responseHeaders("application/json; charset=utf-8"), Body: raw}
}
