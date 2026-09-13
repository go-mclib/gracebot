package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sashabaranov/go-openai"
)

type completionFunc func(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error)

func (f completionFunc) CreateChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	return f(ctx, req)
}

func completionMessage(message openai.ChatCompletionMessage) openai.ChatCompletionResponse {
	return openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{Message: message}}}
}

func TestMentionRouting(t *testing.T) {
	dir := t.TempDir()
	store, err := openPlayerStore(filepath.Join(dir, "players.json"), filepath.Join(dir, "admins.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var received []string
	api := completionFunc(func(_ context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		received = append(received, req.Messages[len(req.Messages)-1].Content)
		return completionMessage(openai.ChatCompletionMessage{Content: "Hello."}), nil
	})
	var replies []string
	a := newAssistant(api, "model", "", newToolRegistry(), func(_ context.Context, message string) error {
		replies = append(replies, message)
		if len(replies) == 4 {
			cancel()
		}
		return nil
	})
	router := chatRouter{assistant: a, store: store, aliases: parseAliases(" , GrAcE, , HELPER,grace ")}
	router.playerChat(ctx, "NewName", "Alice", "!hangman")
	router.playerChat(ctx, "NewName", "Alice", "e")
	router.playerChat(ctx, "NewName", "Alice", "OldName, hello")
	router.playerChat(ctx, "NewName", "newname", "grace, hello")
	router.playerChat(ctx, "NewName", "Alice", "hey nEwNaMe!")
	router.playerChat(ctx, "NewName", "Bob", "GRACE, lets play hangman")
	router.playerChat(ctx, "NewName", "Bob", "can HELPER help?")
	// The requested rule is substring containment, not whole-word matching.
	router.playerChat(ctx, "NewName", "Alice", "prefixNewNamesuffix")
	a.run(ctx)
	if len(received) != 4 || received[0] != "hey nEwNaMe!" || received[1] != "GRACE, lets play hangman" || received[2] != "can HELPER help?" || received[3] != "prefixNewNamesuffix" {
		t.Fatalf("AI received messages without a valid mention: %q", received)
	}
	if !strings.HasPrefix(replies[1], "Bob: ") {
		t.Fatalf("reply not addressed to requesting player: %q", replies[1])
	}
	if mentioned("grace, hello", "NewName", parseAliases(" , ")) || mentioned("anything", "", nil) {
		t.Fatal("empty aliases or username matched arbitrary chat")
	}
}

func TestToolConversationRetainsCompleteTurns(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := openPlayerStore(filepath.Join(dir, "players.json"), filepath.Join(dir, "admins.json"))
	if err != nil {
		t.Fatal(err)
	}
	games := newGameManager(store)
	tools := newToolRegistry(games.tools())
	api := completionFunc(func(_ context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		pending := map[string]bool{}
		users := 0
		for _, message := range req.Messages {
			if message.Role == openai.ChatMessageRoleUser {
				users++
				if strings.Contains(req.Messages[0].Content, `"requesting_player":"Bob"`) && strings.Contains(message.Content, "Alice") {
					t.Fatal("another player's conversation leaked")
				}
			}
			for _, call := range message.ToolCalls {
				pending[call.ID] = true
			}
			if message.Role == openai.ChatMessageRoleTool {
				if !pending[message.ToolCallID] {
					t.Fatalf("orphaned tool response %s", message.ToolCallID)
				}
				delete(pending, message.ToolCallID)
			}
		}
		if len(pending) != 0 {
			t.Fatal("AI request has unanswered tool calls")
		}
		if users > maxHistoryTurns+1 {
			t.Fatal("conversation history is unbounded")
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role == openai.ChatMessageRoleUser {
			return completionMessage(openai.ChatCompletionMessage{ToolCalls: []openai.ToolCall{{ID: fmt.Sprintf("status-%d", users), Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "game_status", Arguments: `{}`}}}}), nil
		}
		var result gameResult
		if err := json.Unmarshal([]byte(last.Content), &result); err != nil {
			t.Fatal(err)
		}
		if result.Status != "none" {
			t.Fatalf("status result changed: %+v", result)
		}
		return completionMessage(openai.ChatCompletionMessage{Content: "No game is active."}), nil
	})
	a := newAssistant(api, "model", "", tools, nil)
	for range maxHistoryTurns + 3 {
		if _, err := a.respond(ctx, chatRequest{sender: "Alice", username: "GraceBot", message: "Grace, Alice wants the game status"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.respond(ctx, chatRequest{sender: "Bob", username: "GraceBot", message: "Grace, game status?"}); err != nil {
		t.Fatal(err)
	}
}

func TestToolLoopIsBounded(t *testing.T) {
	calls := 0
	tools := newToolRegistry([]toolSpec{makeTool("action", "A stateful action", nil, nil, func(context.Context, string, json.RawMessage) (any, error) {
		calls++
		return "done", nil
	})})
	api := completionFunc(func(_ context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		return completionMessage(openai.ChatCompletionMessage{ToolCalls: []openai.ToolCall{{ID: fmt.Sprintf("call-%d", calls), Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "action", Arguments: `{}`}}}}), nil
	})
	a := newAssistant(api, "model", "", tools, nil)
	_, err := a.respond(context.Background(), chatRequest{sender: "Alice", username: "GraceBot", message: "grace, do it"})
	if err == nil || calls != maxToolRounds {
		t.Fatalf("unbounded or unexpected tool effects: calls=%d err=%v", calls, err)
	}
}

func TestChatChunksPreserveUnicodeAndClientLimit(t *testing.T) {
	message := strings.Repeat("漢字🌍", 100)
	chunks := chatChunks(message)
	if strings.Join(chunks, "") != message {
		t.Fatal("multibyte chat was corrupted or lost")
	}
	for _, chunk := range chunks {
		if !utf8.ValidString(chunk) || len(chunk) > 256 {
			t.Fatalf("invalid Minecraft chat chunk: %q", chunk)
		}
	}
	if got := strings.Join(chatChunks("hello\x00\nworld§!"), ""); got != "hello world!" {
		t.Fatalf("unsafe control text: %q", got)
	}
}

func TestOverloadedMentionsReceiveBusyReply(t *testing.T) {
	dir := t.TempDir()
	store, err := openPlayerStore(filepath.Join(dir, "players.json"), filepath.Join(dir, "admins.json"))
	if err != nil {
		t.Fatal(err)
	}
	var replies []string
	a := newAssistant(nil, "model", "", newToolRegistry(), func(_ context.Context, message string) error {
		replies = append(replies, message)
		return nil
	})
	router := chatRouter{assistant: a, store: store}
	ctx := context.Background()
	// Keep the worker stopped to model a full request queue deterministically.
	for range cap(a.requests) {
		router.playerChat(ctx, "Nova", "Alice", "Nova, hello")
	}
	router.playerChat(ctx, "Nova", "Bob", "!hangman")
	router.playerChat(ctx, "Nova", "Bob", "hello everyone")
	if len(replies) != 0 {
		t.Fatal("overload made unmentioned chat trigger a response")
	}
	router.playerChat(ctx, "Nova", "Bob", "NOVA, hello")
	if len(replies) != 1 || !strings.HasPrefix(replies[0], "Bob: ") {
		t.Fatalf("overloaded mention was silently dropped: %q", replies)
	}
}
