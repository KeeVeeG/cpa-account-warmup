package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"cliproxy-account-warmup/internal/warmup"
)

const (
	defaultCoreURL   = "http://127.0.0.1:8317"
	executionHeader  = "X-CLIProxyAPI-Warmup-Ticket"
	executionPrefix  = "cpa-warmup."
	executionModel   = "cliproxy-account-warmup-internal"
	ticketLifetime   = 30 * time.Second
	maxTickets       = 64
	maxResponseBody  = 1 << 20
	executionTimeout = 2 * time.Minute
)

type executionRequest struct {
	EntryProtocol  string      `json:"entry_protocol"`
	ExitProtocol   string      `json:"exit_protocol"`
	Model          string      `json:"model"`
	Stream         bool        `json:"stream"`
	Body           []byte      `json:"body"`
	Headers        http.Header `json:"headers"`
	AuthID         string      `json:"auth_id"`
	ForcedProvider string      `json:"forced_provider"`
	HostCallbackID string      `json:"host_callback_id,omitempty"`
}

type executionResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	Body       []byte      `json:"body"`
}

type executionTicket struct {
	request       executionRequest
	body          []byte
	path          string
	expires       time.Time
	authenticated bool
	started       bool
	completed     bool
	done          chan struct{}
}

// historyHost enters the normal API request lifecycle before delegating to the
// native executor. Its callback then inherits the request ID used by Usage.
type historyHost struct {
	native  warmup.Host
	client  *http.Client
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	url     string
	enabled bool
	closed  bool
	running int
	active  sync.WaitGroup
	tickets map[[32]byte]*executionTicket
	now     func() time.Time
}

func newHistoryHost(native warmup.Host) *historyHost {
	ctx, cancel := context.WithCancel(context.Background())
	return &historyHost{
		native: native, ctx: ctx, cancel: cancel, url: defaultCoreURL,
		tickets: make(map[[32]byte]*executionTicket), now: time.Now,
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:                 nil,
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				TLSHandshakeTimeout:   5 * time.Second,
				ResponseHeaderTimeout: executionTimeout,
			},
			Timeout:       executionTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func validateCoreURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return "", errors.New("core_url must be an HTTP or HTTPS loopback origin with an explicit port")
	}
	address := net.ParseIP(u.Hostname())
	port, err := strconv.Atoi(u.Port())
	if address == nil || !address.IsLoopback() || err != nil || port < 1 || port > 65535 {
		return "", errors.New("core_url must use a literal loopback IP address and a port from 1 to 65535")
	}
	return u.Scheme + "://" + net.JoinHostPort(address.String(), strconv.Itoa(port)), nil
}

func (h *historyHost) close() {
	h.mu.Lock()
	h.closed = true
	clear(h.tickets)
	h.cancel()
	h.mu.Unlock()
	h.client.CloseIdleConnections()
	h.active.Wait()
}

func (h *historyHost) Call(method string, request, target any) error {
	h.mu.Lock()
	enabled, closed := h.enabled, h.closed
	h.mu.Unlock()
	if closed {
		return errors.New("The warmup transport is closed")
	}
	if method != "host.model.execute" || !enabled {
		if h.native == nil {
			return errors.New("The CLIProxyAPI host is unavailable")
		}
		return h.native.Call(method, request, target)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return errors.New("Could not prepare the warmup request")
	}
	var prepared executionRequest
	if err := json.Unmarshal(raw, &prepared); err != nil {
		return errors.New("Could not prepare the warmup request")
	}
	path := "/v1/responses"
	protocol := "openai-response"
	if prepared.ForcedProvider == "claude" {
		path, protocol = "/v1/messages", "claude"
	} else if prepared.ForcedProvider != "codex" {
		return errors.New("Unsupported warmup provider")
	}
	if prepared.AuthID == "" || prepared.Model == "" || prepared.Stream || prepared.EntryProtocol != protocol || prepared.ExitProtocol != protocol || len(prepared.Body) == 0 || len(prepared.Body) > maxRequestBody {
		return errors.New("Invalid warmup execution request")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(prepared.Body, &payload); err != nil || payload == nil {
		return errors.New("Invalid warmup payload")
	}
	payload["model"], _ = json.Marshal(executionModel)
	body, err := json.Marshal(payload)
	if err != nil || len(body) > maxRequestBody {
		return errors.New("Invalid warmup payload")
	}
	random, err := randomToken()
	if err != nil {
		return errors.New("Could not prepare a warmup ticket")
	}
	token := executionPrefix + random
	key := sha256.Sum256([]byte(token))
	h.mu.Lock()
	now := h.now()
	for key, ticket := range h.tickets {
		if !ticket.expires.After(now) {
			delete(h.tickets, key)
		}
	}
	if h.closed || !h.enabled || len(h.tickets) >= maxTickets {
		h.mu.Unlock()
		return errors.New("The warmup transport is unavailable")
	}
	ticket := &executionTicket{request: prepared, body: body, path: path, expires: now.Add(ticketLifetime), done: make(chan struct{})}
	h.tickets[key] = ticket
	coreURL := h.url
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.tickets, key)
		started := ticket.started
		h.mu.Unlock()
		// A disconnected HTTP caller does not prove the native request stopped.
		// Keep the engine's account guard until its actual callback has returned.
		// Removing an unstarted ticket under the same lock prevents a late start.
		if started {
			<-ticket.done
		}
	}()

	upstream, err := http.NewRequestWithContext(h.ctx, http.MethodPost, coreURL+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("Could not prepare the local API request")
	}
	upstream.Header = prepared.Headers.Clone()
	upstream.Header.Set("Authorization", "Bearer "+token)
	upstream.Header.Set(executionHeader, token)
	response, err := h.client.Do(upstream)
	if err != nil {
		return errors.New("The local API request did not complete; its outcome may be unknown")
	}
	defer response.Body.Close()
	body, err = io.ReadAll(io.LimitReader(response.Body, maxResponseBody+1))
	if err != nil || len(body) > maxResponseBody {
		return errors.New("The local API returned an invalid response")
	}
	h.mu.Lock()
	completed := ticket.completed
	h.mu.Unlock()
	if response.StatusCode >= 200 && response.StatusCode < 300 && !completed {
		return errors.New("The local API did not execute the prepared warmup request")
	}
	encoded, err := json.Marshal(executionResponse{StatusCode: response.StatusCode, Headers: response.Header, Body: body})
	if err != nil {
		return errors.New("Could not encode the local API response")
	}
	return json.Unmarshal(encoded, target)
}

type frontendAuthRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Query   url.Values
	Body    []byte
}

type frontendAuthResponse struct {
	Authenticated bool
	Principal     string
}

type routeRequest struct {
	RequestedModel string
	Headers        http.Header
}

type routeResponse struct {
	Handled    bool
	TargetKind string
}

type executorRequest struct {
	SourceFormat    string
	Model           string
	Stream          bool
	Headers         http.Header
	OriginalRequest []byte
	Payload         []byte
	Metadata        map[string]any
	HostCallbackID  string `json:"host_callback_id"`
}

type executorResponse struct {
	Payload []byte
	Headers http.Header
}

func oneHeader(headers http.Header, name string) (string, bool) {
	var values []string
	for key, entries := range headers {
		if strings.EqualFold(key, name) {
			values = append(values, entries...)
		}
	}
	if len(values) != 1 {
		return "", false
	}
	return values[0], true
}

func executionToken(headers http.Header) (string, bool) {
	token, single := oneHeader(headers, executionHeader)
	authorization, authSingle := oneHeader(headers, "Authorization")
	scheme, credential, hasCredential := strings.Cut(authorization, " ")
	ours := strings.HasPrefix(token, executionPrefix) || (hasCredential && strings.EqualFold(scheme, "Bearer") && strings.HasPrefix(credential, executionPrefix))
	for name := range headers {
		ours = ours || strings.EqualFold(name, executionHeader)
	}
	if !single || !authSingle || !strings.HasPrefix(token, executionPrefix) || len(token) != len(executionPrefix)+64 || authorization != "Bearer "+token {
		return "", ours
	}
	return token, true
}

func (h *historyHost) authenticate(req frontendAuthRequest) frontendAuthResponse {
	token, _ := executionToken(req.Headers)
	if token == "" || req.Method != http.MethodPost || len(req.Query) != 0 || len(req.Body) > maxRequestBody {
		return frontendAuthResponse{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ticket := h.tickets[sha256.Sum256([]byte(token))]
	if h.closed || !h.enabled || ticket == nil || ticket.authenticated || !ticket.expires.After(h.now()) || req.Path != ticket.path || !bytes.Equal(req.Body, ticket.body) {
		return frontendAuthResponse{}
	}
	ticket.authenticated = true
	return frontendAuthResponse{Authenticated: true, Principal: "Account Warmup"}
}

type executionError struct{ status int }

func (e executionError) Error() string   { return "The warmup ticket could not be executed" }
func (e executionError) StatusCode() int { return e.status }

func (h *historyHost) route(req routeRequest) routeResponse {
	_, ours := executionToken(req.Headers)
	if req.RequestedModel == executionModel || ours {
		return routeResponse{Handled: true, TargetKind: "self"}
	}
	return routeResponse{}
}

func (h *historyHost) execute(req executorRequest) (result executorResponse, err error) {
	token, ours := executionToken(req.Headers)
	defer func() {
		if recover() != nil {
			result, err = executorResponse{}, executionError{http.StatusBadGateway}
		}
	}()
	if !ours || token == "" || req.HostCallbackID == "" || req.Stream || len(req.Payload) > maxRequestBody || len(req.OriginalRequest) > maxRequestBody {
		return result, executionError{http.StatusForbidden}
	}
	h.mu.Lock()
	key := sha256.Sum256([]byte(token))
	ticket := h.tickets[key]
	valid := !h.closed && h.enabled && ticket != nil && ticket.authenticated && ticket.expires.After(h.now())
	if valid {
		path, _ := req.Metadata["request_path"].(string)
		valid = req.Model == executionModel && req.SourceFormat == ticket.request.EntryProtocol && path == ticket.path && bytes.Equal(req.Payload, ticket.body) && bytes.Equal(req.OriginalRequest, ticket.body)
	}
	if !valid {
		h.mu.Unlock()
		return result, executionError{http.StatusForbidden}
	}
	delete(h.tickets, key)
	ticket.started = true
	h.running++
	h.active.Add(1)
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.running--
		close(ticket.done)
		h.mu.Unlock()
		h.active.Done()
	}()

	prepared := ticket.request
	prepared.HostCallbackID = req.HostCallbackID
	var response executionResponse
	if h.native == nil {
		return result, executionError{http.StatusServiceUnavailable}
	}
	if err := h.native.Call("host.model.execute", prepared, &response); err != nil {
		status := http.StatusBadGateway
		var upstream interface{ StatusCode() int }
		if errors.As(err, &upstream) && upstream.StatusCode() >= 400 && upstream.StatusCode() <= 599 {
			status = upstream.StatusCode()
		}
		return result, executionError{status}
	}
	if response.StatusCode < 200 || response.StatusCode > 599 || len(response.Body) > maxResponseBody {
		return result, executionError{http.StatusBadGateway}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return result, executionError{response.StatusCode}
	}
	h.mu.Lock()
	ticket.completed = true
	h.mu.Unlock()
	return executorResponse{Payload: response.Body, Headers: http.Header{"Content-Type": {"application/json"}}}, nil
}
