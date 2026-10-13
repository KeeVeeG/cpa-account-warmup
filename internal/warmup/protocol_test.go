package warmup

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestBuildModelRequestPinsProviderAccountAndModel(t *testing.T) {
	for _, test := range []struct{ provider, model, protocol string }{
		{"claude", "claude-haiku-5-5", "claude"},
		{"codex", "gpt-6-luna", "openai-response"},
	} {
		t.Run(test.provider, func(t *testing.T) {
			request, err := buildModelRequest(test.provider, test.model, "account-exact-id", "callback-123")
			if err != nil {
				t.Fatal(err)
			}
			if request.AuthID != "account-exact-id" || request.ForcedProvider != test.provider || request.Model != test.model || request.HostCallbackID != "callback-123" {
				t.Fatalf("lost account routing constraints: %#v", request)
			}
			if request.Stream || request.EntryProtocol != test.protocol || request.ExitProtocol != test.protocol {
				t.Fatalf("unexpected host protocol: %#v", request)
			}
			if len(request.Headers) == 0 || len(request.Headers["Content-Type"]) != 1 {
				t.Fatal("explicit headers are needed to prevent management-header inheritance")
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"auth_id", "forced_provider", "entry_protocol", "exit_protocol", "host_callback_id"} {
				if len(wire[field]) == 0 {
					t.Fatalf("missing host ABI field %s", field)
				}
			}
			if strings.Contains(strings.ToLower(string(encoded)), "authorization") || strings.Contains(string(encoded), "$TOKEN$") {
				t.Fatal("model requests must not contain credential material")
			}
		})
	}
}

func TestBuildModelRequestBoundsClaudeAndUsesSupportedCodexFields(t *testing.T) {
	claude, err := buildModelRequest("claude", "claude-haiku-5-5", "id", "")
	if err != nil {
		t.Fatal(err)
	}
	var claudeBody struct {
		Model        string                           `json:"model"`
		MaxTokens    int                              `json:"max_tokens"`
		Thinking     struct{ Type string }            `json:"thinking"`
		OutputConfig json.RawMessage                  `json:"output_config"`
		Messages     []struct{ Role, Content string } `json:"messages"`
	}
	if err := json.Unmarshal(claude.Body, &claudeBody); err != nil {
		t.Fatal(err)
	}
	if claudeBody.Model != "claude-haiku-5-5" || claudeBody.MaxTokens != 1 || claudeBody.Thinking.Type != "disabled" || len(claudeBody.OutputConfig) != 0 || len(claudeBody.Messages) != 1 || claudeBody.Messages[0].Role != "user" || claudeBody.Messages[0].Content != "Reply OK" {
		t.Fatalf("unexpected Claude generation budget: %s", claude.Body)
	}
	if http.Header(claude.Headers).Get("X-OpenAI-Internal-Codex-Responses-Lite") != "" {
		t.Fatal("Codex-specific headers must not be sent to Claude")
	}

	codex, err := buildModelRequest("codex", "gpt-6-luna", "id", "")
	if err != nil {
		t.Fatal(err)
	}
	var codexBody map[string]any
	if err := json.Unmarshal(codex.Body, &codexBody); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens", "temperature", "top_p", "previous_response_id"} {
		if _, present := codexBody[forbidden]; present {
			t.Fatalf("Codex request contains unsupported or unnecessary %s", forbidden)
		}
	}
	if codexBody["store"] != false || codexBody["stream"] != false || codexBody["tool_choice"] != "none" || codexBody["model"] != "gpt-6-luna" {
		t.Fatalf("unexpected Codex request: %s", codex.Body)
	}
	if effort := codexBody["reasoning"].(map[string]any)["effort"]; effort != "none" {
		t.Fatalf("warmup should disable reasoning: %v", effort)
	}
	if codexBody["input"] != "Reply OK" || codexBody["instructions"] != "" || codexBody["text"].(map[string]any)["verbosity"] != "low" {
		t.Fatalf("warmup should contain one short prompt: %s", codex.Body)
	}
	if http.Header(codex.Headers).Get("X-OpenAI-Internal-Codex-Responses-Lite") != "true" {
		t.Fatal("Codex requests should prevent automatic tool injection")
	}
	if len(codexBody["tools"].([]any)) != 0 || len(codex.Body) > 512 {
		t.Fatal("warmup should not contain tools, history, or a large prompt")
	}
}

func TestValidateModelResponseKeepsReportedReasoning(t *testing.T) {
	body := strings.Replace(validCodexCompletion, `"output_tokens":2`, `"output_tokens":25,"output_tokens_details":{"reasoning_tokens":23}`, 1)
	body = strings.Replace(body, `"object":"response"`, `"object":"response","reasoning":{"effort":"low"}`, 1)
	info, err := validateModelResponse("codex", "gpt-6-luna", modelResponse{StatusCode: 200, Body: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	if info.OutputTokens != 25 || info.ReasoningTokens == nil || *info.ReasoningTokens != 23 || info.ReasoningEffort != "low" {
		t.Fatalf("lost the provider's reported reasoning: %#v", info)
	}
}

func TestValidateModelResponseRejectsInvalidReasoningUsage(t *testing.T) {
	for _, details := range []string{`{"reasoning_tokens":-1}`, `{"reasoning_tokens":3}`} {
		body := strings.Replace(validCodexCompletion, `"output_tokens":2`, `"output_tokens":2,"output_tokens_details":`+details, 1)
		if _, err := validateModelResponse("codex", "gpt-6-luna", modelResponse{StatusCode: 200, Body: []byte(body)}); err == nil {
			t.Fatalf("accepted invalid reasoning usage: %s", details)
		}
	}
}

func completionWithUsage(t *testing.T, provider, usage string) modelResponse {
	t.Helper()
	body := validCodexCompletion
	if provider == "claude" {
		body = validClaudeCompletion
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	fields["usage"] = json.RawMessage(usage)
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return modelResponse{StatusCode: http.StatusOK, Body: encoded}
}

func TestValidateModelResponsePreservesUnknownReasoning(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		model := DefaultConfig().ClaudeModel
		if provider == "codex" {
			model = DefaultConfig().CodexModel
		}
		for _, detail := range []string{"", `,"output_tokens_details":{}`, `,"output_tokens_details":{"reasoning_tokens":null}`} {
			response := completionWithUsage(t, provider, `{"input_tokens":6,"output_tokens":1`+detail+`}`)
			info, err := validateModelResponse(provider, model, response)
			if err != nil {
				t.Fatal(err)
			}
			if info.ReasoningTokens != nil {
				t.Fatalf("%s inferred reasoning usage from %q: %#v", provider, detail, info)
			}
		}
	}
}

func TestValidateModelResponseParsesReportedThinking(t *testing.T) {
	for _, test := range []struct {
		name, provider, detail string
		want                   int64
	}{
		{"Claude zero", "claude", `"output_tokens_details":{"thinking_tokens":0}`, 0},
		{"Claude thinking", "claude", `"output_tokens_details":{"thinking_tokens":4}`, 4},
		{"Claude preferred field", "claude", `"output_tokens_details":{"thinking_tokens":4,"reasoning_tokens":2}`, 4},
		{"Claude legacy nested", "claude", `"output_tokens_details":{"reasoning_tokens":3}`, 3},
		{"Claude legacy top-level", "claude", `"thinking_tokens":2`, 2},
		{"Codex zero", "codex", `"output_tokens_details":{"reasoning_tokens":0}`, 0},
		{"Codex reasoning", "codex", `"output_tokens_details":{"reasoning_tokens":4}`, 4},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := DefaultConfig().ClaudeModel
			if test.provider == "codex" {
				model = DefaultConfig().CodexModel
			}
			response := completionWithUsage(t, test.provider, `{"input_tokens":6,"output_tokens":10,`+test.detail+`}`)
			info, err := validateModelResponse(test.provider, model, response)
			if err != nil || info.ReasoningTokens == nil || *info.ReasoningTokens != test.want {
				t.Fatalf("reasoning = %#v, error = %v", info, err)
			}
		})
	}
}

func TestValidateModelResponseCountsCachedInputByProvider(t *testing.T) {
	claude := completionWithUsage(t, "claude", `{"input_tokens":6,"output_tokens":1,"cache_read_input_tokens":100,"cache_creation_input_tokens":40}`)
	info, err := validateModelResponse("claude", DefaultConfig().ClaudeModel, claude)
	if err != nil || info.InputTokens != 146 || info.CacheReadTokens == nil || *info.CacheReadTokens != 100 || info.CacheCreationTokens == nil || *info.CacheCreationTokens != 40 {
		t.Fatalf("Claude usage = %#v, error = %v", info, err)
	}

	codex := completionWithUsage(t, "codex", `{"input_tokens":146,"output_tokens":1,"input_tokens_details":{"cached_tokens":100}}`)
	info, err = validateModelResponse("codex", DefaultConfig().CodexModel, codex)
	if err != nil || info.InputTokens != 146 || info.CacheReadTokens == nil || *info.CacheReadTokens != 100 || info.CacheCreationTokens != nil {
		t.Fatalf("Codex usage = %#v, error = %v", info, err)
	}
}

func TestValidateModelResponseRejectsInvalidTokenBreakdowns(t *testing.T) {
	for _, test := range []struct{ name, provider, usage string }{
		{"Claude negative cache read", "claude", `{"input_tokens":6,"output_tokens":1,"cache_read_input_tokens":-1}`},
		{"Claude negative cache creation", "claude", `{"input_tokens":6,"output_tokens":1,"cache_creation_input_tokens":-1}`},
		{"Claude read overflow", "claude", `{"input_tokens":6,"output_tokens":1,"cache_read_input_tokens":9223372036854775807}`},
		{"Claude creation overflow", "claude", `{"input_tokens":6,"output_tokens":1,"cache_read_input_tokens":9223372036854775801,"cache_creation_input_tokens":1}`},
		{"Claude negative thinking", "claude", `{"input_tokens":6,"output_tokens":1,"output_tokens_details":{"thinking_tokens":-1}}`},
		{"Claude excess thinking", "claude", `{"input_tokens":6,"output_tokens":1,"output_tokens_details":{"thinking_tokens":2}}`},
		{"Claude negative legacy thinking", "claude", `{"input_tokens":6,"output_tokens":1,"thinking_tokens":-1}`},
		{"Codex negative cached", "codex", `{"input_tokens":6,"output_tokens":1,"input_tokens_details":{"cached_tokens":-1}}`},
		{"Codex excess cached", "codex", `{"input_tokens":6,"output_tokens":1,"input_tokens_details":{"cached_tokens":7}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := DefaultConfig().ClaudeModel
			if test.provider == "codex" {
				model = DefaultConfig().CodexModel
			}
			response := completionWithUsage(t, test.provider, test.usage)
			if info, err := validateModelResponse(test.provider, model, response); err == nil {
				t.Fatalf("accepted invalid breakdown: %#v", info)
			}
		})
	}
}

func TestBuildModelRequestRejectsUnpinnedOrUnsupportedTargets(t *testing.T) {
	for _, test := range []struct{ provider, model, authID string }{
		{"other", "model", "id"}, {"claude", " ", "id"}, {"codex", "model", " "},
	} {
		if _, err := buildModelRequest(test.provider, test.model, test.authID, ""); err == nil {
			t.Fatalf("accepted invalid target: %#v", test)
		}
	}
}

const validClaudeCompletion = `{"id":"msg_123","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":6,"output_tokens":1}}`
const validCodexCompletion = `{"id":"resp_123","object":"response","model":"gpt-6-luna","status":"completed","error":null,"incomplete_details":null,"output":[{"type":"reasoning","summary":[]},{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":9,"output_tokens":2}}`

func TestValidateModelResponseAcceptsCompletedGenerations(t *testing.T) {
	for _, test := range []struct {
		provider, model, body string
		input, output         int64
	}{
		{"claude", "claude-haiku-5-5", validClaudeCompletion, 6, 1},
		{"claude", "claude-haiku-5-5", strings.Replace(validClaudeCompletion, `"end_turn"`, `"max_tokens"`, 1), 6, 1},
		{"claude", "claude-haiku-5-5", strings.Replace(validClaudeCompletion, `{"type":"text","text":"OK"}`, `{"type":"thinking","thinking":"","signature":"opaque-signature"},{"type":"text","text":"OK"}`, 1), 6, 1},
		{"claude", "claude-haiku-5-5", strings.Replace(strings.Replace(validClaudeCompletion, `{"type":"text","text":"OK"}`, `{"type":"thinking","thinking":"","signature":"opaque-signature"}`, 1), `"end_turn"`, `"max_tokens"`, 1), 6, 1},
		{"claude", "claude-haiku-5-5", strings.Replace(validClaudeCompletion, `{"type":"text","text":"OK"}`, `{"type":"redacted_thinking","data":"opaque-data"}`, 1), 6, 1},
		{"codex", "gpt-6-luna", validCodexCompletion, 9, 2},
	} {
		t.Run(test.provider+test.body[:10], func(t *testing.T) {
			info, err := validateModelResponse(test.provider, test.model, modelResponse{StatusCode: 200, Body: []byte(test.body)})
			if err != nil {
				t.Fatal(err)
			}
			if info.Model != test.model || info.InputTokens != test.input || info.OutputTokens != test.output {
				t.Fatalf("unexpected usage: %#v", info)
			}
		})
	}
}

func TestValidateModelResponseRejectsFailuresAndIncompleteStreams(t *testing.T) {
	for _, test := range []struct {
		name, provider, model, body string
		status                      int
	}{
		{"http error", "codex", "gpt-6-luna", validCodexCompletion, 429},
		{"empty", "codex", "gpt-6-luna", "", 200},
		{"invalid JSON", "codex", "gpt-6-luna", "not JSON", 200},
		{"SSE is not a completion", "codex", "gpt-6-luna", "data: " + validCodexCompletion, 200},
		{"Codex incomplete", "codex", "gpt-6-luna", strings.Replace(validCodexCompletion, `"status":"completed"`, `"status":"incomplete"`, 1), 200},
		{"Codex error", "codex", "gpt-6-luna", strings.Replace(validCodexCompletion, `"error":null`, `"error":{"message":"private upstream detail"}`, 1), 200},
		{"Codex incomplete detail", "codex", "gpt-6-luna", strings.Replace(validCodexCompletion, `"incomplete_details":null`, `"incomplete_details":{"reason":"max_output_tokens"}`, 1), 200},
		{"Codex other model", "codex", "gpt-6-luna", strings.Replace(validCodexCompletion, "gpt-6-luna", "private-fallback-model", 1), 200},
		{"Codex function", "codex", "gpt-6-luna", strings.Replace(validCodexCompletion, `"type":"message"`, `"type":"function_call"`, 1), 200},
		{"Codex refusal", "codex", "gpt-6-luna", strings.Replace(validCodexCompletion, `"type":"output_text"`, `"type":"refusal"`, 1), 200},
		{"Claude other model", "claude", "claude-haiku-5-5", strings.Replace(validClaudeCompletion, "claude-haiku-5-5", "private-fallback-model", 1), 200},
		{"Claude unfinished", "claude", "claude-haiku-5-5", strings.Replace(validClaudeCompletion, `"stop_reason":"end_turn"`, `"stop_reason":null`, 1), 200},
		{"Claude tools", "claude", "claude-haiku-5-5", strings.Replace(validClaudeCompletion, `"type":"text"`, `"type":"tool_use"`, 1), 200},
		{"Claude empty thinking", "claude", "claude-haiku-5-5", strings.Replace(validClaudeCompletion, `"type":"text"`, `"type":"thinking"`, 1), 200},
		{"no text", "claude", "claude-haiku-5-5", strings.Replace(validClaudeCompletion, `"text":"OK"`, `"text":" "`, 1), 200},
		{"no usage", "claude", "claude-haiku-5-5", strings.Replace(validClaudeCompletion, `"usage":{"input_tokens":6,"output_tokens":1}`, `"usage":null`, 1), 200},
		{"zero usage", "claude", "claude-haiku-5-5", strings.Replace(validClaudeCompletion, `"output_tokens":1`, `"output_tokens":0`, 1), 200},
		{"negative usage", "claude", "claude-haiku-5-5", strings.Replace(validClaudeCompletion, `"input_tokens":6`, `"input_tokens":-1`, 1), 200},
		{"fractional usage", "claude", "claude-haiku-5-5", strings.Replace(validClaudeCompletion, `"input_tokens":6`, `"input_tokens":1.5`, 1), 200},
		{"missing usage count", "claude", "claude-haiku-5-5", strings.Replace(validClaudeCompletion, `"input_tokens":6`, `"cache_read_input_tokens":6`, 1), 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := validateModelResponse(test.provider, test.model, modelResponse{StatusCode: test.status, Body: []byte(test.body)})
			if err == nil {
				t.Fatal("unexpectedly accepted an unverified generation")
			}
			if strings.Contains(err.Error(), "private") || (test.body != "" && strings.Contains(err.Error(), test.body)) {
				t.Fatal("validation must not echo upstream error details")
			}
		})
	}
}

func TestValidateModelResponseBoundsResponseSize(t *testing.T) {
	body := strings.Replace(validClaudeCompletion, `"text":"OK"`, `"text":"`+strings.Repeat("x", maxCompletionBytes)+`"`, 1)
	if _, err := validateModelResponse("claude", "claude-haiku-5-5", modelResponse{StatusCode: 200, Body: []byte(body)}); err == nil {
		t.Fatal("accepted an oversized warmup response")
	}
}
