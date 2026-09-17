package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"
)

type nativeWireCache struct {
	Type string `json:"type"`
	TTL  string `json:"ttl"`
}

type nativeWireBlock struct {
	Type         string           `json:"type"`
	Text         string           `json:"text"`
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	Input        json.RawMessage  `json:"input"`
	ToolUseID    string           `json:"tool_use_id"`
	Content      json.RawMessage  `json:"content"`
	CacheControl *nativeWireCache `json:"cache_control"`
}

type nativeWireMessage struct {
	Role    string            `json:"role"`
	Content []nativeWireBlock `json:"content"`
}

type nativeWireRequest struct {
	System   []nativeWireBlock   `json:"system"`
	Messages []nativeWireMessage `json:"messages"`
	Tools    []struct {
		Name         string           `json:"name"`
		InputSchema  json.RawMessage  `json:"input_schema"`
		CacheControl *nativeWireCache `json:"cache_control"`
	} `json:"tools"`
	ToolChoice   map[string]any   `json:"tool_choice"`
	CacheControl *nativeWireCache `json:"cache_control"`
}

const nativeWireOK = `{"id":"msg_test","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}`

func nativeWireRead(t *testing.T, r *http.Request) (nativeWireRequest, []byte) {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		t.Errorf("native request endpoint: %s %s", r.Method, r.URL.Path)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read request: %v", err)
	}
	var request nativeWireRequest
	if err := json.Unmarshal(body, &request); err != nil {
		t.Errorf("decode native request: %v", err)
	}
	return request, body
}

func nativeWireReply(w http.ResponseWriter, response string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, response)
}

func nativeWireSameJSON(t *testing.T, got, want string) bool {
	t.Helper()
	decode := func(raw string) any {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("invalid structured JSON %q: %v", raw, err)
		}
		return value
	}
	return reflect.DeepEqual(decode(got), decode(want))
}

func nativeWireTools() []openai.Tool {
	return []openai.Tool{
		{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
			Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"filters":{"type":"object"}},"required":["filters"]}`),
		}},
		{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
			Name: "respond", Parameters: json.RawMessage(`{"type":"object","properties":{"messages":{"type":"array","items":{"type":"string"}}},"required":["messages"]}`),
		}},
	}
}

func nativeWireCompletion() completionRequest {
	return completionRequest{ChatCompletionRequest: openai.ChatCompletionRequest{
		Model: "claude-test", MaxTokens: 128, Tools: nativeWireTools(),
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "Static instructions and personality."},
			{Role: openai.ChatMessageRoleSystem, Content: "Authenticated actor: Alice."},
			{Role: openai.ChatMessageRoleUser, Content: "Earlier question."},
			{Role: openai.ChatMessageRoleAssistant, Content: "Earlier answer."},
			{Role: openai.ChatMessageRoleSystem, Content: "Timestamp: 2026-09-17T10:00:00Z; current weather: rain; event: 41."},
			{Role: openai.ChatMessageRoleUser, Content: "Current question."},
		},
	}, historyEnd: 4}
}

func nativeWireCacheCount(t *testing.T, body []byte) int {
	t.Helper()
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	var visit func(any) int
	visit = func(value any) int {
		count := 0
		switch value := value.(type) {
		case map[string]any:
			for key, child := range value {
				if key == "cache_control" {
					count++
					cache, ok := child.(map[string]any)
					if !ok || cache["type"] != "ephemeral" || (cache["ttl"] != nil && cache["ttl"] != "5m") {
						t.Errorf("expected default 5m ephemeral cache, got %v", child)
					}
				}
				count += visit(child)
			}
		case []any:
			for _, child := range value {
				count += visit(child)
			}
		}
		return count
	}
	return visit(root)
}

func TestAnthropicWireCachingKeepsVolatileDataAfterCachedHistory(t *testing.T) {
	type captured struct {
		request nativeWireRequest
		body    []byte
	}
	requests := make(chan captured, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, body := nativeWireRead(t, r)
		requests <- captured{request, body}
		nativeWireReply(w, nativeWireOK)
	}))
	defer server.Close()
	client := newAnthropicClient("test-key", server.URL+"/v1")
	request := nativeWireCompletion()
	var first nativeWireRequest
	for i := range 2 {
		if i == 1 {
			request.Messages[4].Content = "Timestamp: 2026-09-17T10:01:00Z; current weather: sun; event: 42."
			request.Messages[5].Content = "Different current question."
		}
		if _, err := client.CreateChatCompletion(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		got := <-requests
		if count := nativeWireCacheCount(t, got.body); count != 4 {
			t.Fatalf("expected three explicit breakpoints plus automatic tail, got %d: %s", count, got.body)
		}
		if got.request.CacheControl == nil || len(got.request.Tools) != 2 || got.request.Tools[1].CacheControl == nil ||
			len(got.request.System) != 2 || got.request.System[0].CacheControl == nil ||
			len(got.request.Messages) < 3 || len(got.request.Messages[1].Content) == 0 || got.request.Messages[1].Content[len(got.request.Messages[1].Content)-1].CacheControl == nil {
			t.Fatalf("missing tool/static/history/automatic cache layers: %s", got.body)
		}
		if !strings.Contains(string(got.body), request.Messages[4].Content) || !strings.Contains(string(got.body), request.Messages[5].Content) {
			t.Fatalf("current data disappeared from request: %s", got.body)
		}
		if i == 0 {
			first = got.request
		} else if !reflect.DeepEqual(first.Tools, got.request.Tools) || !reflect.DeepEqual(first.System, got.request.System) ||
			!reflect.DeepEqual(first.Messages[:2], got.request.Messages[:2]) {
			t.Fatal("changing only current input invalidated a cached prefix")
		}
	}
	for _, boundary := range []int{0, 1} {
		request.historyEnd = boundary
		if _, err := client.CreateChatCompletion(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		got := <-requests
		if count := nativeWireCacheCount(t, got.body); count != 3 || got.request.CacheControl == nil {
			t.Errorf("history boundary %d added an absent/duplicate history breakpoint or lost automatic caching: %s", boundary, got.body)
		}
	}
}

func TestAnthropicWireParallelToolsAndForcedResponse(t *testing.T) {
	requests := make(chan nativeWireRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, _ := nativeWireRead(t, r)
		requests <- request
		nativeWireReply(w, `{"id":"msg_tools","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"Private analysis."},{"type":"tool_use","id":"next_a","name":"lookup","input":{"filters":{"id":9007199254740993,"enabled":true,"tags":["a",null]}}},{"type":"tool_use","id":"next_b","name":"respond","input":{"messages":["Hello"]}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":5}}`)
	}))
	defer server.Close()
	request := nativeWireCompletion()
	arguments := `{"filters":{"id":9007199254740993,"enabled":true,"tags":["a",null]}}`
	request.Messages = append(request.Messages[:3],
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "Checking both.", ToolCalls: []openai.ToolCall{
			{ID: "call_a", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "lookup", Arguments: arguments}},
			{ID: "call_b", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "lookup", Arguments: `{"filters":{}}`}},
		}},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "call_a", Content: ""},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "call_b", Content: `{"found":[1,2]}`},
	)
	request.historyEnd = len(request.Messages)
	request.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": "respond"}}
	response, err := newAnthropicClient("test-key", server.URL+"/v1").CreateChatCompletion(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	got := <-requests
	if len(got.Tools) != 2 || got.Tools[0].Name != "lookup" || got.Tools[1].Name != "respond" || got.ToolChoice["type"] != "tool" || got.ToolChoice["name"] != "respond" {
		t.Fatalf("forced response discarded tool definitions or failed to select respond: %+v", got)
	}
	for i, tool := range request.Tools {
		schema, err := json.Marshal(tool.Function.Parameters)
		if err != nil {
			t.Fatal(err)
		}
		if !nativeWireSameJSON(t, string(got.Tools[i].InputSchema), string(schema)) {
			t.Errorf("forced response changed schema for tool %q", tool.Function.Name)
		}
	}
	if len(got.Messages) != 3 || got.Messages[1].Role != "assistant" || got.Messages[2].Role != "user" {
		t.Fatalf("parallel tool exchange was split or reordered: %+v", got.Messages)
	}
	uses, results := got.Messages[1].Content, got.Messages[2].Content
	if len(uses) != 3 || uses[0].Type != "text" || uses[0].Text != "Checking both." ||
		uses[1].Type != "tool_use" || uses[1].ID != "call_a" || uses[1].Name != "lookup" ||
		uses[2].Type != "tool_use" || uses[2].ID != "call_b" || uses[2].Name != "lookup" {
		t.Fatalf("lost assistant text or parallel tool calls: %+v", uses)
	}
	if !nativeWireSameJSON(t, string(uses[1].Input), arguments) || !nativeWireSameJSON(t, string(uses[2].Input), `{"filters":{}}`) {
		t.Fatalf("raw structured tool arguments changed: %s / %s", uses[1].Input, uses[2].Input)
	}
	if len(results) != 2 || results[0].Type != "tool_result" || results[0].ToolUseID != "call_a" ||
		results[1].Type != "tool_result" || results[1].ToolUseID != "call_b" {
		t.Fatalf("lost tool result IDs or empty tool result: %+v", results)
	}
	for i, want := range []string{"", `{"found":[1,2]}`} {
		if len(results[i].Content) == 0 && want == "" {
			continue
		}
		var text string
		if err := json.Unmarshal(results[i].Content, &text); err != nil {
			var blocks []nativeWireBlock
			if err := json.Unmarshal(results[i].Content, &blocks); err != nil {
				t.Fatalf("invalid tool result content: %s", results[i].Content)
			}
			for _, block := range blocks {
				if block.Type != "text" {
					t.Fatalf("unexpected tool result block: %+v", block)
				}
				text += block.Text
			}
		}
		if text != want {
			t.Errorf("tool result %d: got %q, want %q", i, text, want)
		}
	}
	if len(response.Choices) != 1 {
		t.Fatalf("native response yielded %d choices", len(response.Choices))
	}
	message := response.Choices[0].Message
	if message.Role != openai.ChatMessageRoleAssistant || message.Content != "Private analysis." || len(message.ToolCalls) != 2 {
		t.Fatalf("native response lost text or parallel calls: %+v", message)
	}
	for i, want := range []struct{ id, name, arguments string }{
		{"next_a", "lookup", arguments}, {"next_b", "respond", `{"messages":["Hello"]}`},
	} {
		call := message.ToolCalls[i]
		if call.ID != want.id || call.Type != openai.ToolTypeFunction || call.Function.Name != want.name || !nativeWireSameJSON(t, call.Function.Arguments, want.arguments) {
			t.Errorf("converted tool call %d: %+v", i, call)
		}
	}
}

func TestAnthropicWireFailuresAreNotSwallowedOrRetried(t *testing.T) {
	t.Run("provider error", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"wire-provider-overloaded"}}`)
		}))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := newAnthropicClient("test-key", server.URL+"/v1").CreateChatCompletion(ctx, nativeWireCompletion())
		if err == nil || !strings.Contains(err.Error(), "wire-provider-overloaded") {
			t.Fatalf("provider error was swallowed or replaced: %v", err)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("provider error triggered %d requests; want exactly one", got)
		}
	})
	t.Run("in-flight cancellation", func(t *testing.T) {
		var calls atomic.Int32
		started := make(chan struct{}, 1)
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		defer server.Close()
		defer close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := newAnthropicClient("test-key", server.URL+"/v1").CreateChatCompletion(ctx, nativeWireCompletion())
			done <- err
		}()
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("native request never reached server")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation was swallowed or replaced: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("native request ignored cancellation")
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("cancellation triggered %d requests; want exactly one", got)
		}
	})
}

func TestGenericProviderKeepsOpenAIWireProtocol(t *testing.T) {
	requests := make(chan map[string]json.RawMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("generic provider endpoint: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode generic request: %v", err)
		}
		requests <- body
		nativeWireReply(w, `{"id":"chat_test","object":"chat.completion","model":"generic-model","choices":[{"index":0,"message":{"role":"assistant","content":"generic answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer server.Close()
	response, err := newChatCompletionClient(config{apiKey: "test-key", apiBase: server.URL + "/v1"}).CreateChatCompletion(context.Background(), nativeWireCompletion())
	if err != nil {
		t.Fatal(err)
	}
	body := <-requests
	for _, field := range []string{"cache_control", "system", "historyEnd", "history_end"} {
		if _, exists := body[field]; exists {
			t.Errorf("native/internal field %q leaked into generic provider request", field)
		}
	}
	var messages []openai.ChatCompletionMessage
	if err := json.Unmarshal(body["messages"], &messages); err != nil || !reflect.DeepEqual(messages, nativeWireCompletion().Messages) {
		t.Fatalf("generic provider messages changed: %s (error %v)", body["messages"], err)
	}
	if len(response.Choices) != 1 || response.Choices[0].Message.Content != "generic answer" {
		t.Fatalf("generic provider response changed: %+v", response)
	}
}
