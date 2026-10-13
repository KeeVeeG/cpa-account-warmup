package warmup

import (
	"bytes"
	"encoding/json"
	"errors"
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
	InputTokens  int64
	OutputTokens int64
	Model        string
}

// buildModelRequest asks the host to use one exact credential and model. It
// deliberately supplies no upstream credentials or endpoint overrides.
func buildModelRequest(provider, model, authID, callbackID string) (modelRequest, error) {
	if strings.TrimSpace(model) == "" || strings.TrimSpace(authID) == "" {
		return modelRequest{}, errors.New("A model and an exact account ID are required.")
	}

	var protocol string
	var body any
	switch provider {
	case "claude":
		protocol = "claude"
		body = map[string]any{
			"model":      model,
			"max_tokens": 32,
			"stream":     false,
			// Low is Haiku 5.5's lowest effort. Adaptive thinking preserves it
			// through the host; thinking and visible text share the 32-token cap.
			"thinking":      map[string]string{"type": "adaptive"},
			"output_config": map[string]string{"effort": "low"},
			"messages": []map[string]string{
				{"role": "user", "content": "Reply with OK."},
			},
		}
	case "codex":
		protocol = "openai-response"
		body = map[string]any{
			"model":        model,
			"stream":       false,
			"store":        false,
			"instructions": "Reply with OK only.",
			"input":        "Reply with OK.",
			// gpt-6-luna's lowest supported effort in the pinned host catalog
			// is low. Neither "none" nor an output token cap is supported by
			// the Codex backend. Keep this limitation visible in the UI/docs.
			"reasoning":   map[string]string{"effort": "low"},
			"tools":       []any{},
			"tool_choice": "none",
		}
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
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
			"Accept":       {"application/json"},
		},
	}, nil
}

type responseUsage struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
}

type responseContent struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
	Data      string `json:"data"`
}

type completionBody struct {
	ID                string            `json:"id"`
	Type              string            `json:"type"`
	Object            string            `json:"object"`
	Role              string            `json:"role"`
	Model             string            `json:"model"`
	Status            string            `json:"status"`
	StopReason        string            `json:"stop_reason"`
	Error             json.RawMessage   `json:"error"`
	IncompleteDetails json.RawMessage   `json:"incomplete_details"`
	Usage             *responseUsage    `json:"usage"`
	Content           []responseContent `json:"content"`
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
	return modelResponseInfo{Model: body.Model, InputTokens: *body.Usage.InputTokens, OutputTokens: *body.Usage.OutputTokens}, nil
}

func hasJSONValue(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) != 0 && !bytes.Equal(trimmed, []byte("null"))
}
