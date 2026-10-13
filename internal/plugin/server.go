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

	"cliproxy-account-warmup/internal/warmup"
	"gopkg.in/yaml.v3"
)

const ID = "cliproxy-account-warmup"
const Version = "0.2.0"
const APIBase = "/v0/management/plugins/" + ID
const ResourceBase = "/v0/resource/plugins/" + ID

// The host requires this field even for unpublished, locally built plugins.
// A release build can set the public repository URL with -ldflags -X.
var RepositoryURL = "local:" + ID

// CLIProxyAPI v8.0.27 still registers custom plugin routes under /v0/management.
// Its built-in configuration routes use /v8/management instead.

//go:embed web/*
var assets embed.FS

type Server struct {
	engine    *warmup.Engine
	mu        sync.Mutex
	local     *localServer
	closed    bool
	closeOnce sync.Once
}

func New(host warmup.Host) *Server {
	return &Server{engine: warmup.New(host)}
}

type Envelope struct {
	OK     bool      `json:"ok"`
	Result any       `json:"result,omitempty"`
	Error  *RPCError `json:"error,omitempty"`
}

type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
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
		return ErrorEnvelope("plugin_error", err.Error())
	}
	b, err := json.Marshal(Envelope{OK: true, Result: result})
	if err != nil {
		return ErrorEnvelope("serialization_error", "Could not encode the plugin response.")
	}
	return b
}

func ErrorEnvelope(code, message string) []byte {
	b, _ := json.Marshal(Envelope{Error: &RPCError{Code: code, Message: message}})
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
		if err := s.engine.Configure(cfg); err != nil {
			return nil, err
		}
		if method == "plugin.register" {
			if err := s.startLocal(); err != nil {
				return nil, errors.New("Could not start the Account Warmup interface.")
			}
		}
		return registration(), nil
	case "plugin.shutdown":
		s.Close()
		return struct{}{}, nil
	case "management.register":
		resources := []map[string]string{
			{"Path": "/status"},
			{"Path": "/app.js"},
			{"Path": "/styles.css"},
		}
		s.mu.Lock()
		if s.local != nil && !s.closed {
			resources = append(resources, map[string]string{
				"Path": s.local.launchPath, "Menu": "Account Warmup",
				"Description": "Warm up Claude or Codex accounts individually.",
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

func parseConfig(raw []byte) (warmup.Config, error) {
	cfg := warmup.DefaultConfig()
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
			err = node.Decode(&cfg.CooldownSeconds)
		case "enabled", "priority", "store":
			continue
		default:
			return cfg, errors.New("Unknown plugin configuration field.")
		}
		if err != nil {
			return cfg, errors.New("Invalid plugin configuration field type.")
		}
	}
	return cfg, nil
}

func registration() any {
	return map[string]any{
		"schema_version": 6,
		"metadata": map[string]any{
			"Name": "Account Warmup", "Version": Version,
			"Author": "Account Warmup contributors", "GitHubRepository": RepositoryURL,
			"ConfigFields": []map[string]string{
				{"Name": "claude_model", "Type": "string", "Description": "Exact Claude model ID. Default: claude-haiku-5-5. No automatic fallback."},
				{"Name": "codex_model", "Type": "string", "Description": "Exact Codex model ID. Default: gpt-6-luna. No automatic fallback."},
				{"Name": "cooldown_seconds", "Type": "integer", "Description": "Local duplicate protection after a request. Default: 300 seconds. This is not the subscription reset time."},
			},
		},
		"capabilities": map[string]bool{"management_api": true},
	}
}

func (s *Server) management(req managementRequest) managementResponse {
	if req.Method == "GET" {
		s.mu.Lock()
		local := s.local
		s.mu.Unlock()
		if local != nil && req.Path == ResourceBase+local.launchPath {
			return local.launch()
		}
		var name, contentType string
		switch req.Path {
		case ResourceBase + "/status":
			name, contentType = "index.html", "text/html; charset=utf-8"
		case ResourceBase + "/app.js":
			name, contentType = "app.js", "text/javascript; charset=utf-8"
		case ResourceBase + "/styles.css":
			name, contentType = "styles.css", "text/css; charset=utf-8"
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
