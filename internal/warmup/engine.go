// Package warmup performs explicitly requested, credential-pinned warmup calls.
package warmup

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	maxRecentResults = 200
	maxRequestCache  = 1024
	maxConcurrent    = 16
	requestLifetime  = 24 * time.Hour
)

var (
	modelPattern   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
	requestPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{7,127}$`)
)

// Host invokes the native CLIProxyAPI host callbacks. Implementations must be
// safe for concurrent calls and must not retain request-scoped callback IDs.
type Host interface {
	Call(method string, request any, response any) error
}

// Config controls the explicit model choices.
type Config struct {
	ClaudeModel string `json:"claude_model"`
	CodexModel  string `json:"codex_model"`
}

func DefaultConfig() Config {
	return Config{ClaudeModel: "claude-haiku-5-5", CodexModel: "gpt-6-luna"}
}

// Account contains only the metadata needed for the UI. In particular, the
// host's account, path and status_message fields are never returned to clients.
type Account struct {
	ID          string `json:"id"`
	Provider    string `json:"provider"`
	Label       string `json:"label"`
	Disabled    bool   `json:"disabled"`
	Unavailable bool   `json:"unavailable"`
	Eligible    bool   `json:"eligible"`
	Reason      string `json:"reason,omitempty"`
}

type WarmRequest struct {
	Provider  string `json:"provider"`
	AuthID    string `json:"auth_id"`
	RequestID string `json:"request_id"`
}

// Result records only locally observed request outcomes. Success does not
// establish that a particular subscription quota window has started.
type Result struct {
	RequestID           string    `json:"request_id"`
	AuthID              string    `json:"auth_id"`
	Provider            string    `json:"provider"`
	Model               string    `json:"model"`
	Status              string    `json:"status"`
	Message             string    `json:"message"`
	StartedAt           time.Time `json:"started_at"`
	FinishedAt          time.Time `json:"finished_at"`
	InputTokens         int64     `json:"input_tokens,omitempty"`
	OutputTokens        int64     `json:"output_tokens,omitempty"`
	CacheReadTokens     *int64    `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens *int64    `json:"cache_creation_tokens,omitempty"`
	ReasoningTokens     *int64    `json:"reasoning_tokens,omitempty"`
	ReasoningEffort     string    `json:"reasoning_effort,omitempty"`
	Duplicate           bool      `json:"duplicate,omitempty"`
}

type State struct {
	Config   Config    `json:"config"`
	Accounts []Account `json:"accounts"`
	Results  []Result  `json:"results"`
}

type authEntry struct {
	ID             string    `json:"id"`
	Provider       string    `json:"provider"`
	Type           string    `json:"type"`
	Label          string    `json:"label"`
	Email          string    `json:"email"`
	Name           string    `json:"name"`
	AccountType    string    `json:"account_type"`
	Status         string    `json:"status"`
	Disabled       bool      `json:"disabled"`
	Unavailable    bool      `json:"unavailable"`
	NextRetryAfter time.Time `json:"next_retry_after"`
}

type authList struct {
	Files []authEntry `json:"files"`
}

type cacheEntry struct {
	request WarmRequest
	result  Result
	running bool
	added   time.Time
}

// Engine keeps bounded, in-memory guards. Restarting/reloading the plugin
// clears its history; the server remains authoritative about actual quotas.
type Engine struct {
	host     Host
	mu       sync.Mutex
	config   Config
	active   map[string]bool
	requests map[string]*cacheEntry
	recent   []Result
	now      func() time.Time
}

// New constructs an engine with default settings. Configure validates and
// applies any external configuration before the engine is exposed to clients.
func New(host Host) *Engine {
	return &Engine{
		host:     host,
		config:   DefaultConfig(),
		active:   make(map[string]bool),
		requests: make(map[string]*cacheEntry),
		recent:   make([]Result, 0),
		now:      time.Now,
	}
}

func normalizeConfig(config Config) (Config, error) {
	if !modelPattern.MatchString(config.ClaudeModel) || !strings.HasPrefix(config.ClaudeModel, "claude-") {
		return Config{}, errors.New("Claude model must be a literal claude- model ID without routing or reasoning suffixes")
	}
	if !modelPattern.MatchString(config.CodexModel) || !strings.HasPrefix(config.CodexModel, "gpt-") {
		return Config{}, errors.New("Codex model must be a literal gpt- model ID without routing or reasoning suffixes")
	}
	return config, nil
}

func (e *Engine) Configure(config Config) error {
	config, err := normalizeConfig(config)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.config = config
	return nil
}

func (e *Engine) listAccounts(callbackID string) ([]authEntry, error) {
	if e.host == nil {
		return nil, errors.New("The CLIProxyAPI host is unavailable")
	}
	var response authList
	err := e.callHost("host.auth.list", struct {
		CallbackID string `json:"host_callback_id,omitempty"`
	}{callbackID}, &response)
	if err != nil {
		return nil, errors.New("Could not read accounts from CLIProxyAPI")
	}
	return response.Files, nil
}

// A native host adapter may recover its own errors, but the engine also guards
// this boundary so an unexpected callback panic cannot strand an account lock.
func (e *Engine) callHost(method string, request, response any) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("The host callback failed unexpectedly")
		}
	}()
	return e.host.Call(method, request, response)
}

func (e *Engine) State(callbackID string) (State, error) {
	entries, err := e.listAccounts(callbackID)
	if err != nil {
		return State{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now().UTC()
	e.pruneLocked(now)
	state := State{Config: e.config, Accounts: make([]Account, 0), Results: append([]Result{}, e.recent...)}
	counts := make(map[string]int)
	for _, entry := range entries {
		counts[entry.ID]++
	}
	for _, entry := range entries {
		provider := entryProvider(entry)
		if !supportedProvider(provider) {
			continue
		}
		a := accountFromEntry(entry, now)
		if counts[entry.ID] != 1 {
			a.Eligible, a.Reason = false, "Ambiguous account ID"
		}
		if e.active[entry.ID] {
			a.Eligible, a.Reason = false, "A warmup request is in progress"
		}
		state.Accounts = append(state.Accounts, a)
	}
	sort.Slice(state.Accounts, func(i, j int) bool {
		a, b := state.Accounts[i], state.Accounts[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		return a.ID < b.ID
	})
	return state, nil
}

func validateWarmRequest(request WarmRequest) error {
	if !supportedProvider(request.Provider) {
		return errors.New("provider must be claude or codex")
	}
	if !validAuthID(request.AuthID) {
		return errors.New("A valid account ID is required")
	}
	if !requestPattern.MatchString(request.RequestID) {
		return errors.New("request_id must contain 8 to 128 letters, digits, dots, underscores or hyphens")
	}
	return nil
}

func (e *Engine) Warm(request WarmRequest, callbackID string) (Result, error) {
	if err := validateWarmRequest(request); err != nil {
		return Result{}, err
	}
	e.mu.Lock()
	now := e.now().UTC()
	e.pruneLocked(now)
	if cached, ok := e.requests[request.RequestID]; ok {
		if cached.request != request {
			e.mu.Unlock()
			return Result{}, errors.New("request_id was already used for another account")
		}
		result := cached.result
		result.Duplicate = true
		if cached.running {
			result.Status, result.Message = "skipped", "This request is already in progress"
		}
		e.mu.Unlock()
		return result, nil
	}
	config := e.config
	model := config.ClaudeModel
	if request.Provider == "codex" {
		model = config.CodexModel
	}
	result := Result{RequestID: request.RequestID, AuthID: request.AuthID, Provider: request.Provider, Model: model, Status: "skipped", StartedAt: now, FinishedAt: now}
	skip := func(message string) (Result, error) {
		result.Message = message
		if e.makeCacheRoomLocked() {
			e.requests[request.RequestID] = &cacheEntry{request: request, result: result, added: now}
		}
		e.mu.Unlock()
		return result, nil
	}
	if e.active[request.AuthID] {
		return skip("A warmup request is already in progress for this account")
	}
	if len(e.active) >= maxConcurrent {
		return skip("Too many warmup requests are in progress")
	}
	if !e.makeCacheRoomLocked() {
		return skip("The request guard is full; wait for an active request to finish")
	}
	e.requests[request.RequestID] = &cacheEntry{request: request, result: result, running: true, added: now}
	e.active[request.AuthID] = true
	e.mu.Unlock()

	finish := func() Result {
		e.mu.Lock()
		defer e.mu.Unlock()
		result.FinishedAt = e.now().UTC()
		delete(e.active, request.AuthID)
		e.requests[request.RequestID].running = false
		e.requests[request.RequestID].result = result
		e.recent = append([]Result{result}, e.recent...)
		if len(e.recent) > maxRecentResults {
			e.recent = e.recent[:maxRecentResults]
		}
		return result
	}

	entries, err := e.listAccounts(callbackID)
	if err != nil {
		result.Status, result.Message = "error", "Could not recheck accounts; no model request was sent"
		return finish(), nil
	}
	var selected *authEntry
	for i := range entries {
		if entries[i].ID != request.AuthID {
			continue
		}
		if selected != nil {
			result.Message = "Ambiguous account ID; no model request was sent"
			return finish(), nil
		}
		selected = &entries[i]
	}
	if selected == nil {
		result.Message = "The account is no longer available"
		return finish(), nil
	}
	if entryProvider(*selected) != request.Provider {
		result.Message = "The account does not belong to the selected provider"
		return finish(), nil
	}
	account := accountFromEntry(*selected, e.now().UTC())
	if !account.Eligible {
		result.Message = account.Reason
		return finish(), nil
	}
	modelReq, err := buildModelRequest(request.Provider, model, request.AuthID, callbackID)
	if err != nil {
		result.Status, result.Message = "error", "The model request configuration is invalid"
		return finish(), nil
	}
	var response modelResponse
	err = e.callHost("host.model.execute", modelReq, &response)
	if err != nil {
		result.Status, result.Message = hostFailure(err)
		return finish(), nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		result.Status, result.Message = failureForStatus(response.StatusCode)
		return finish(), nil
	}
	info, err := validateModelResponse(request.Provider, model, response)
	if err != nil {
		result.Status, result.Message = "uncertain", "A response was received, but the requested model and completion could not be verified; quota may have been used"
		return finish(), nil
	}
	result.Status, result.Message = "success", "The requested model completed a response; the quota window has not been verified"
	result.InputTokens, result.OutputTokens = info.InputTokens, info.OutputTokens
	result.CacheReadTokens, result.CacheCreationTokens = info.CacheReadTokens, info.CacheCreationTokens
	result.ReasoningTokens, result.ReasoningEffort = info.ReasoningTokens, info.ReasoningEffort
	return finish(), nil
}

func hostFailure(err error) (string, string) {
	var status interface{ StatusCode() int }
	if errors.As(err, &status) {
		return failureForStatus(status.StatusCode())
	}
	return failureForStatus(0)
}

func failureForStatus(status int) (string, string) {
	switch status {
	case 400, 404, 422:
		return "error", "The provider rejected the request or model; check the configured model"
	case 401, 403:
		return "error", "The provider rejected this account's authorization"
	case 429:
		return "error", "The account is rate limited; no fallback was requested"
	}
	return "uncertain", "The request outcome could not be verified; quota may have been used"
}

func supportedProvider(provider string) bool { return provider == "claude" || provider == "codex" }

func entryProvider(entry authEntry) string {
	if entry.Provider != "" {
		return strings.ToLower(strings.TrimSpace(entry.Provider))
	}
	return strings.ToLower(strings.TrimSpace(entry.Type))
}

func validAuthID(id string) bool {
	if id == "" || len(id) > 512 || strings.TrimSpace(id) != id {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func accountFromEntry(entry authEntry, now time.Time) Account {
	label := entry.Label
	if strings.TrimSpace(label) == "" {
		label = entry.Email
	}
	if strings.TrimSpace(label) == "" {
		label = entry.Name
	}
	if strings.TrimSpace(label) == "" {
		label = "Unnamed account"
	}
	// The host exposes its raw last-failure flag. Like the native scheduler,
	// allow another attempt after an explicit recovery timestamp has passed.
	// The host still enforces model-specific quota and authorization checks.
	unavailable := entry.Unavailable && (entry.NextRetryAfter.IsZero() || entry.NextRetryAfter.After(now))
	a := Account{ID: entry.ID, Provider: entryProvider(entry), Label: safeLabel(label), Disabled: entry.Disabled || entry.Status == "disabled", Unavailable: unavailable, Eligible: true}
	switch {
	case !validAuthID(entry.ID):
		a.Eligible, a.Reason = false, "No usable runtime account ID"
	case strings.ToLower(entry.AccountType) != "oauth":
		a.Eligible, a.Reason = false, "Only OAuth subscription accounts are supported"
	case a.Disabled:
		a.Eligible, a.Reason = false, "Account is disabled"
	case a.Unavailable:
		a.Eligible, a.Reason = false, "Account is unavailable in CLIProxyAPI"
	}
	return a
}

func safeLabel(label string) string {
	runes := []rune(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, label))
	if len(runes) > 160 {
		runes = runes[:160]
	}
	return strings.TrimSpace(string(runes))
}

func (e *Engine) pruneLocked(now time.Time) {
	for id, entry := range e.requests {
		if !entry.running && now.Sub(entry.added) >= requestLifetime {
			delete(e.requests, id)
		}
	}
}

func (e *Engine) makeCacheRoomLocked() bool {
	if len(e.requests) < maxRequestCache {
		return true
	}
	var oldestID string
	var oldest time.Time
	for id, entry := range e.requests {
		if !entry.running && (oldestID == "" || entry.added.Before(oldest)) {
			oldestID, oldest = id, entry.added
		}
	}
	if oldestID == "" {
		return false
	}
	delete(e.requests, oldestID)
	return true
}
