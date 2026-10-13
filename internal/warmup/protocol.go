package warmup

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
)

// The host accepts a non-streaming request for both protocols. Its Codex
// executor consumes the upstream SSE stream and returns the terminal response.
type modelRequest struct {
	EntryProtocol  string              `json:"entry_protocol"`
	ExitProtocol   string              `json:"exit_protocol"`
	Model          string              `json:"model"`
	Stream         bool                `json:"stream"`
	Body           []byte              `json:"body"`
	Headers        map[string][]string `json:"headers"`
	AuthID         string              `json:"auth_id"`
	ForcedProvider string              `json:"forced_provider"`
	HostCallbackID string              `json:"host_callback_id,omitempty"`
}

type modelResponse struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers"`
	Body       []byte              `json:"body"`
}

type modelResponseInfo struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     *int64
	CacheCreationTokens *int64
	ReasoningTokens     *int64
	ReasoningEffort     string
	Model               string
}

// buildModelRequest asks the host to use one exact credential and model. It
// deliberately supplies no upstream credentials or endpoint overrides.
func buildModelRequest(provider, model, authID, callbackID string) (modelRequest, error) {
	if strings.TrimSpace(model) == "" || strings.TrimSpace(authID) == "" {
		return modelRequest{}, errors.New("A model and an exact account ID are required.")
	}

	var protocol string
	var body any
	headers := http.Header{
		"Content-Type": {"application/json"},
		"Accept":       {"application/json"},
	}
	switch provider {
	case "claude":
		protocol = "claude"
		body = map[string]any{
			"model":      model,
			"max_tokens": 1,
			"stream":     false,
			"thinking":   map[string]string{"type": "disabled"},
			"messages": []map[string]string{
				{"role": "user", "content": "Reply OK"},
			},
		}
	case "codex":
		protocol = "openai-response"
		body = map[string]any{
			"model":        model,
			"stream":       false,
			"store":        false,
			"instructions": "",
			"input":        "Reply OK",
			// The Codex backend rejects output token limits. Request no reasoning
			// and retain the reported effort: older host catalogs may raise it.
			"reasoning":   map[string]string{"effort": "none"},
			"text":        map[string]string{"verbosity": "low"},
			"tools":       []any{},
			"tool_choice": "none",
		}
		// Keep the host from adding its image-generation tool to this request.
		headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
	default:
		return modelRequest{}, errors.New("Only Claude and Codex accounts are supported.")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return modelRequest{}, errors.New("The warmup request could not be prepared.")
	}
	return modelRequest{
		EntryProtocol: protocol, ExitProtocol: protocol, Model: model,
		Body: encoded, AuthID: authID, ForcedProvider: provider,
		HostCallbackID: callbackID,
		// An explicit nonempty header map prevents host fallback from
		// inheriting the browser's management authorization headers.
		Headers: headers,
	}, nil
}

type responseUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	ThinkingTokens           *int64 `json:"thinking_tokens"`
	InputTokensDetails       *struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
		ThinkingTokens  *int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

type responseContent struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
	Data      string `json:"data"`
}

type completionBody struct {
	ID                string                  `json:"id"`
	Type              string                  `json:"type"`
	Object            string                  `json:"object"`
	Role              string                  `json:"role"`
	Model             string                  `json:"model"`
	Status            string                  `json:"status"`
	StopReason        string                  `json:"stop_reason"`
	Error             json.RawMessage         `json:"error"`
	IncompleteDetails json.RawMessage         `json:"incomplete_details"`
	Usage             *responseUsage          `json:"usage"`
	Reasoning         struct{ Effort string } `json:"reasoning"`
	Content           []responseContent       `json:"content"`
	Output            []struct {
		Type    string            `json:"type"`
		Role    string            `json:"role"`
		Status  string            `json:"status"`
		Content []responseContent `json:"content"`
	} `json:"output"`
}

const maxCompletionBytes = 1 << 20

// validateModelResponse confirms a completed generation, not a newly opened
// quota window. Never expose raw upstream content through validation errors.
func validateModelResponse(provider, requestedModel string, response modelResponse) (modelResponseInfo, error) {
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return modelResponseInfo{}, errors.New("The provider rejected the warmup request.")
	}
	if len(response.Body) == 0 || len(response.Body) > maxCompletionBytes {
		return modelResponseInfo{}, errors.New("The provider returned an invalid warmup response.")
	}
	var body completionBody
	if err := json.Unmarshal(response.Body, &body); err != nil || strings.TrimSpace(body.ID) == "" {
		return modelResponseInfo{}, errors.New("The provider returned an invalid warmup response.")
	}
	if hasJSONValue(body.Error) {
		return modelResponseInfo{}, errors.New("The provider reported a warmup error.")
	}
	// A route may be changed by another installed host plugin. Treat any
	// different or absent response model as unverified; do not retry it.
	if requestedModel == "" || body.Model != requestedModel {
		return modelResponseInfo{}, errors.New("The response model did not match the requested model. Check the host's model aliases and routing plugins.")
	}

	var generationFound bool
	switch provider {
	case "claude":
		if body.Type != "message" || body.Role != "assistant" || (body.StopReason != "end_turn" && body.StopReason != "max_tokens") {
			return modelResponseInfo{}, errors.New("Claude did not return a completed response.")
		}
		for _, content := range body.Content {
			switch content.Type {
			case "text":
				generationFound = generationFound || strings.TrimSpace(content.Text) != ""
			case "thinking":
				generationFound = generationFound || content.Signature != "" || strings.TrimSpace(content.Thinking) != ""
			case "redacted_thinking":
				generationFound = generationFound || content.Data != ""
			default:
				return modelResponseInfo{}, errors.New("Claude returned unexpected content.")
			}
		}
	case "codex":
		if body.Object != "response" || body.Status != "completed" || hasJSONValue(body.IncompleteDetails) {
			return modelResponseInfo{}, errors.New("Codex did not return a completed response.")
		}
		for _, item := range body.Output {
			if item.Type == "reasoning" {
				continue
			}
			if item.Type != "message" || item.Role != "assistant" || item.Status != "completed" {
				return modelResponseInfo{}, errors.New("Codex returned unexpected non-text output.")
			}
			for _, content := range item.Content {
				if content.Type != "output_text" {
					return modelResponseInfo{}, errors.New("Codex returned unexpected non-text content.")
				}
				generationFound = generationFound || strings.TrimSpace(content.Text) != ""
			}
		}
	default:
		return modelResponseInfo{}, errors.New("Only Claude and Codex accounts are supported.")
	}
	if !generationFound {
		return modelResponseInfo{}, errors.New("The provider did not return generated content.")
	}
	if body.Usage == nil || body.Usage.InputTokens == nil || body.Usage.OutputTokens == nil || *body.Usage.InputTokens < 0 || *body.Usage.OutputTokens <= 0 {
		return modelResponseInfo{}, errors.New("The provider did not report valid generation usage.")
	}
	info := modelResponseInfo{
		Model: body.Model, InputTokens: *body.Usage.InputTokens, OutputTokens: *body.Usage.OutputTokens,
		ReasoningEffort: body.Reasoning.Effort,
	}
	if details := body.Usage.OutputTokensDetails; details != nil {
		info.ReasoningTokens = details.ReasoningTokens
	}
	if provider == "claude" {
		// Messages reports cached input separately from input_tokens. Responses
		// already includes cached input in its total, so only Claude needs a sum.
		info.CacheReadTokens = body.Usage.CacheReadInputTokens
		info.CacheCreationTokens = body.Usage.CacheCreationInputTokens
		for _, tokens := range []*int64{info.CacheReadTokens, info.CacheCreationTokens} {
			if tokens == nil {
				continue
			}
			if *tokens < 0 || *tokens > math.MaxInt64-info.InputTokens {
				return modelResponseInfo{}, errors.New("The provider did not report valid cached input usage.")
			}
			info.InputTokens += *tokens
		}
		if details := body.Usage.OutputTokensDetails; details != nil && details.ThinkingTokens != nil {
			info.ReasoningTokens = details.ThinkingTokens
		} else if info.ReasoningTokens == nil {
			info.ReasoningTokens = body.Usage.ThinkingTokens
		}
	} else if details := body.Usage.InputTokensDetails; details != nil {
		info.CacheReadTokens = details.CachedTokens
		if tokens := info.CacheReadTokens; tokens != nil && (*tokens < 0 || *tokens > info.InputTokens) {
			return modelResponseInfo{}, errors.New("The provider did not report valid cached input usage.")
		}
	}
	if tokens := info.ReasoningTokens; tokens != nil && (*tokens < 0 || *tokens > info.OutputTokens) {
		return modelResponseInfo{}, errors.New("The provider did not report valid reasoning usage.")
	}
	return info, nil
}

func hasJSONValue(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) != 0 && !bytes.Equal(trimmed, []byte("null"))
}
