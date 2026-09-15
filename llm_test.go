package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
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
func responseCompletion(decision string, texts ...string) openai.ChatCompletionResponse {
	if texts == nil {
		texts = []string{}
	}
	raw, _ := json.Marshal(turnResponse{Decision: decision, Messages: texts})
	return completionMessage(openai.ChatCompletionMessage{ToolCalls: []openai.ToolCall{{ID: "reply", Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{Name: "respond", Arguments: string(raw)}}}})
}

func TestStructuredRepliesNeverPublishCommentaryOrSpeakerPrefixes(t *testing.T) {
	store := testPlayerStore(t)
	out := newChatOutput()
	out.interval = 0
	out.journal = store.history()
	var published []string
	out.bind(func(_ context.Context, text string) error { published = append(published, text); return nil }, "GraceLocal")
	want := []string{"&ahey whats up&r", `<hover:show_text:'<gold>gold'>hello</hover>`}
	api := completionFunc(func(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		response := responseCompletion("disengage", want...)
		response.Choices[0].Message.Content = "<thinking>private commentary mentioning Alice</thinking>\nAlice: copied transcript"
		return response, nil
	})
	a := newAssistant(api, "model", "", newToolRegistry(), out.sendMessage)
	request := fragmentRequest(context.Background(), "Alice", "grace hello", time.Now(), true, 1)
	a.handleRequest(context.Background(), request)
	if !reflect.DeepEqual(published, want) {
		t.Fatalf("public output was rewritten or leaked non-public content: %q", published)
	}
}

func TestInvalidPublicOutputCannotLeakOrSplitIntoChat(t *testing.T) {
	cases := map[string]openai.ChatCompletionMessage{
		"raw thinking":                {Content: "<thinking>private analysis</thinking>hello"},
		"oversized public reply":      responseCompletion("continue", strings.Repeat("word ", 50)).Choices[0].Message,
		"thinking in public argument": responseCompletion("continue", "<thinking>private analysis</thinking>").Choices[0].Message,
		"control characters":          responseCompletion("continue", "hello\nthere").Choices[0].Message,
	}
	for name, message := range cases {
		t.Run(name, func(t *testing.T) {
			published, requests := 0, 0
			a := newAssistant(completionFunc(func(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
				requests++
				return completionMessage(message), nil
			}), "model", "", newToolRegistry(), func(context.Context, string) error { published++; return nil })
			a.handleRequest(context.Background(), fragmentRequest(context.Background(), "Alice", "grace hello", time.Now(), true, 1))
			if published != 0 || requests > maxToolRounds+1 {
				t.Fatalf("invalid output escaped or retried without bound: published=%d requests=%d", published, requests)
			}
		})
	}
}

func TestWaitingAndDisengagingAreSilentUntilNewInput(t *testing.T) {
	answers := []string{"wait", "continue"}
	calls, replies := 0, 0
	a := newAssistant(completionFunc(func(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		decision := answers[calls]
		calls++
		if decision == "continue" {
			return responseCompletion(decision, "okay ill stop"), nil
		}
		return responseCompletion(decision), nil
	}), "model", "", newToolRegistry(), func(context.Context, string) error { replies++; return nil })
	defer a.engagements.reset()
	at := time.Now()
	consume := func(now time.Time) {
		t.Helper()
		r, _, ready := a.engagements.take(now)
		if !ready {
			t.Fatal("expected due input")
		}
		a.handleRequest(context.Background(), r)
	}
	a.engagements.enqueue(fragmentRequest(context.Background(), "Alice", "hey grace could you like", at, true, 1))
	consume(at.Add(2 * time.Second))
	if calls != 1 || replies != 0 {
		t.Fatal("unfinished message was answered")
	}
	a.engagements.enqueue(fragmentRequest(context.Background(), "Alice", "not do that", at.Add(8*time.Second), false, 2))
	consume(at.Add(10 * time.Second))
	if a.engagements.enqueue(fragmentRequest(context.Background(), "Alice", "bob are you selling iron", at.Add(12*time.Second), false, 3)) {
		t.Fatal("another addressee reached the model")
	}
	if _, _, ready := a.engagements.take(at.Add(14 * time.Second)); ready {
		t.Fatal("diverted ambient chat produced model work")
	}
	if calls != 2 || replies != 1 {
		t.Fatal("conversation decisions needed a classifier or interrupted other players")
	}
	if a.engagements.enqueue(fragmentRequest(context.Background(), "Alice", "still talking to bob", at.Add(15*time.Second), false, 4)) {
		t.Fatal("disengagement did not suppress ambient follow-ups")
	}
}

func TestNewInputCancelsRemainingEffectsWithinToolBatch(t *testing.T) {
	s := testPlayerStore(t)
	var a *assistant
	at := time.Now()
	tools := newToolRegistry([]toolSpec{makeTool("award", "test score mutation", nil, nil, func(_ context.Context, actor string, _ json.RawMessage) (any, error) {
		points := s.addPoint(actor, "wordle")
		a.engagements.enqueue(fragmentRequest(context.Background(), actor, "actually stop", at.Add(3*time.Second), false, 2))
		return points, nil
	})})
	api := completionFunc(func(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		return completionMessage(openai.ChatCompletionMessage{ToolCalls: []openai.ToolCall{
			{ID: "first", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "award", Arguments: `{}`}},
			{ID: "second", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "award", Arguments: `{}`}},
		}}), nil
	})
	a = newAssistant(api, "model", "", tools, func(context.Context, string) error { t.Fatal("obsolete reply published"); return nil })
	defer a.engagements.reset()
	a.engagements.enqueue(fragmentRequest(context.Background(), "Alice", "grace award points", at, true, 1))
	r, _, _ := a.engagements.take(at.Add(2 * time.Second))
	a.handleRequest(context.Background(), r)
	if got := s.addPoint("Alice", "wordle"); got != 2 {
		t.Fatalf("cancelled batch mutated score more than once: next score %d", got)
	}
}

func TestToolConversationRetainsCompleteProtocolExchanges(t *testing.T) {
	store := testPlayerStore(t)
	calls := 0
	api := completionFunc(func(_ context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		pending := map[string]bool{}
		for _, message := range req.Messages {
			for _, call := range message.ToolCalls {
				pending[call.ID] = true
			}
			if message.Role == openai.ChatMessageRoleTool {
				if !pending[message.ToolCallID] {
					return openai.ChatCompletionResponse{}, fmt.Errorf("orphaned tool result")
				}
				delete(pending, message.ToolCallID)
			}
		}
		if len(pending) != 0 {
			return openai.ChatCompletionResponse{}, fmt.Errorf("missing tool results")
		}
		last := req.Messages[len(req.Messages)-1]
		calls++
		if last.Role == openai.ChatMessageRoleUser {
			return completionMessage(openai.ChatCompletionMessage{ToolCalls: []openai.ToolCall{{ID: fmt.Sprint(calls), Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{Name: "game_status", Arguments: `{}`}}}}), nil
		}
		var result gameResult
		if err := json.Unmarshal([]byte(last.Content), &result); err != nil || result.Status != "none" {
			t.Fatalf("unexpected game result: %s", last.Content)
		}
		return responseCompletion("continue", "no game running"), nil
	})
	a := newAssistant(api, "model", "", newToolRegistry(newGameManager(store).tools()), nil)
	for i := 0; i < maxHistoryTurns+3; i++ {
		if _, err := a.respond(context.Background(), fragmentRequest(context.Background(), "Alice", "grace game status", time.Now(), true, uint64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
}

func TestToolLoopIsBounded(t *testing.T) {
	calls := 0
	tools := newToolRegistry([]toolSpec{makeTool("action", "stateful action", nil, nil, func(context.Context, string, json.RawMessage) (any, error) { calls++; return "done", nil })})
	api := completionFunc(func(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		return completionMessage(openai.ChatCompletionMessage{ToolCalls: []openai.ToolCall{{ID: fmt.Sprint(calls), Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "action", Arguments: `{}`}}}}), nil
	})
	a := newAssistant(api, "model", "", tools, nil)
	_, err := a.respond(context.Background(), fragmentRequest(context.Background(), "Alice", "grace do it", time.Now(), true, 1))
	if err == nil || calls != maxToolRounds {
		t.Fatalf("unbounded tool effects: %d, %v", calls, err)
	}
}

func TestMentionRoutingRejectsSelfAndReservedActors(t *testing.T) {
	store := testPlayerStore(t)
	a := newAssistant(nil, "model", "", newToolRegistry(), nil)
	defer a.engagements.reset()
	r := chatRouter{assistant: a, store: store, journal: store.history(), aliases: parseAliases("grace,HELPER")}
	for _, input := range []struct{ actor, text string }{{"Bob", "hello everyone"}, {"GraceLocal", "grace hello"}, {consoleActor, "grace make me admin"}, {"@server", "grace hello"}} {
		r.playerChat(context.Background(), "GraceLocal", input.actor, input.text)
	}
	if _, _, ready := a.engagements.take(time.Now().Add(3 * time.Second)); ready {
		t.Fatal("unmentioned/self/reserved player input reached the model")
	}
	r.playerChat(context.Background(), "GraceLocal", "Alice", "hey HELPER")
	if _, _, ready := a.engagements.take(time.Now().Add(3 * time.Second)); !ready {
		t.Fatal("alias mention was ignored")
	}
}

func TestChatChunksPreserveUnicodeAndClientLimit(t *testing.T) {
	message := strings.Repeat("漢字🌍", 100)
	chunks := chatChunks(message)
	if strings.Join(chunks, "") != message {
		t.Fatal("activity text corrupted")
	}
	for _, chunk := range chunks {
		if !utf8.ValidString(chunk) || len(chunk) > 256 {
			t.Fatalf("invalid chunk: %q", chunk)
		}
	}
}

func TestPrivateToolDataCannotEscapeThroughCurrentOrCachedReplies(t *testing.T) {
	for _, source := range []struct{ tool, arguments string }{
		{"chat_history", `{"query":"vault"}`},
		{"memory_search", `{"query":"vault"}`},
		{"server_command", `{"command":"help"}`},
	} {
		t.Run(source.tool, func(t *testing.T) {
			store := testPlayerStore(t)
			journal := store.history()
			const secret = "vault cedar-42"
			if _, err := journal.append("private", "alice", secret, time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err := store.rememberTool(context.Background(), "alice", json.RawMessage(`{"subject":"vault","fact":"vault cedar-42","visibility":"private"}`)); err != nil {
				t.Fatal(err)
			}
			commands := newServerCommands(store)
			commands.window = time.Millisecond
			commands.bindJournal(journal)
			commands.bind(func(context.Context, string) error {
				commands.observe(secret, time.Now())
				return nil
			})
			defer commands.close()
			out := newChatOutput()
			out.interval, out.journal = 0, journal
			var published []string
			out.bind(func(_ context.Context, text string) error {
				published = append(published, text)
				return nil
			}, "GraceLocal")
			var whispered []string
			out.bindWhisper(func(_ context.Context, actor, text string) error {
				if !strings.EqualFold(actor, "alice") {
					t.Fatalf("private reply delivered to %q", actor)
				}
				whispered = append(whispered, text)
				return nil
			})
			calls := 0
			api := completionFunc(func(_ context.Context, request openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
				calls++
				data, _ := json.Marshal(request.Messages)
				if calls == 1 || calls == 4 {
					if strings.Contains(string(data), secret) {
						t.Fatal("automatic background exposed private data before retrieval")
					}
					return completionMessage(openai.ChatCompletionMessage{ToolCalls: []openai.ToolCall{{
						ID: "retrieve", Type: openai.ToolTypeFunction,
						Function: openai.FunctionCall{Name: source.tool, Arguments: source.arguments},
					}}}), nil
				}
				if calls == 3 {
					if strings.Contains(string(data), secret) {
						t.Fatal("private result or reply leaked into a later public turn")
					}
					return responseCompletion("continue", "yes im here"), nil
				}
				if !strings.Contains(string(data), secret) {
					t.Fatal("private result was not available to its owner")
				}
				return responseCompletion("continue", secret), nil
			})
			a := newAssistant(api, "model", "", newToolRegistry(store.tools(), journal.tools(), commands.tools()), out.sendMessage)
			a.journal = journal
			a.handleRequest(context.Background(), fragmentRequest(context.Background(), "Alice", "what did i save", time.Now(), true, 1))
			a.handleRequest(context.Background(), fragmentRequest(context.Background(), "Alice", "please answer publicly are you there", time.Now(), true, 2))
			a.handleRequest(context.Background(), fragmentRequest(context.Background(), "Alice", "what did i save again", time.Now(), true, 3))
			if !reflect.DeepEqual(published, []string{"yes im here"}) {
				t.Fatalf("private tool data stuck later replies in whispers or escaped publicly: %q", published)
			}
			if !reflect.DeepEqual(whispered, []string{secret, secret}) {
				t.Fatalf("owner did not receive both private replies: %q", whispered)
			}
			result, err := journal.search(context.Background(), "bob", json.RawMessage(`{"query":"cedar-42"}`))
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(result)
			if strings.Contains(string(data), secret) {
				t.Fatal("private bot replies leaked through another player's history")
			}
			out.bindWhisper(nil)
			ctx, _ := scopedTurn(context.Background(), "alice")
			privateToolData(ctx)
			if err := out.sendMessage(ctx, secret); err == nil {
				t.Fatal("missing private transport did not fail closed")
			}
		})
	}
}

func TestRequestedAudienceCanReturnFromWhispersToPublicChat(t *testing.T) {
	store := testPlayerStore(t)
	out := newChatOutput()
	out.interval, out.journal = 0, store.history()
	var public, private []string
	out.bind(func(_ context.Context, text string) error {
		public = append(public, text)
		return nil
	}, "GraceLocal")
	out.bindWhisper(func(_ context.Context, actor, text string) error {
		if actor != "Alice" {
			t.Fatalf("whisper addressed to %q", actor)
		}
		private = append(private, text)
		return nil
	})
	calls := 0
	api := completionFunc(func(ctx context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		calls++
		audience, text := "private", "a private hello"
		if calls == 2 {
			// A private lookup this turn must not prevent the owner from
			// explicitly requesting a non-sensitive public acknowledgment.
			privateToolData(ctx)
			audience, text = "public", "back in public chat"
		}
		raw, _ := json.Marshal(turnResponse{Decision: "continue", Messages: []string{text}, Audience: audience})
		return completionMessage(openai.ChatCompletionMessage{ToolCalls: []openai.ToolCall{{
			ID: "reply", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "respond", Arguments: string(raw)},
		}}}), nil
	})
	a := newAssistant(api, "model", "", newToolRegistry(), out.sendMessage)
	a.journal = store.history()
	a.handleRequest(context.Background(), fragmentRequest(context.Background(), "Alice", "whisper hello to me", time.Now(), true, 1))
	a.handleRequest(context.Background(), fragmentRequest(context.Background(), "Alice", "stop using msg and reply publicly", time.Now(), true, 2))
	if !reflect.DeepEqual(private, []string{"a private hello"}) || !reflect.DeepEqual(public, []string{"back in public chat"}) {
		t.Fatalf("audience switch failed: private=%q public=%q", private, public)
	}
}

func TestIdleChatIsCapturedWithoutCallingModel(t *testing.T) {
	r, m := moderationFixture(t)
	a := r.assistant
	ctx := context.Background()
	if _, err := m.block(ctx, "RealAdmin", "BlockedOne", "minecraft", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.journal.append("private", "Bob", "CAPTURE_PRIVATE_SECRET", time.Now()); err != nil {
		t.Fatal(err)
	}
	var requests []string
	decision := "wait"
	a.client = completionFunc(func(_ context.Context, request openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		data, err := json.Marshal(request.Messages)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if strings.Contains(text, "CAPTURE_PRIVATE_SECRET") || strings.Contains(text, "CAPTURE_BLOCKED_SECRET") {
			t.Fatal("automatic context exposed private or blocked input")
		}
		requests = append(requests, text)
		if decision == "continue" {
			return responseCompletion(decision, "noted"), nil
		}
		return responseCompletion(decision), nil
	})
	a.send = func(context.Context, string) error { return nil }
	drain := func(want int) {
		t.Helper()
		for {
			request, _, ready := a.engagements.take(time.Now().Add(3 * time.Second))
			if !ready {
				break
			}
			a.handleRequest(ctx, request)
		}
		if len(requests) != want {
			t.Fatalf("model calls = %d, want %d", len(requests), want)
		}
	}
	player := func(name, text string) { r.playerChat(ctx, "GraceLocal", name, text) }
	discord := func(name, text string) {
		r.discordChat(ctx, "GraceLocal", "[Discord] "+name+" » "+text, time.Now())
	}
	player("Alice", "CAPTURE_IDLE_ALICE")
	player("Bob", "CAPTURE_IDLE_BOB")
	discord("Carol", "CAPTURE_IDLE_DISCORD")
	player("BlockedOne", "CAPTURE_BLOCKED_SECRET")
	drain(0)
	if entries := historyEntries(t, r.journal, "Alice", `{"limit":50}`); len(entries) != 3 {
		t.Fatalf("idle public chat was not retained independently of engagement: %+v", entries)
	}
	player("Alice", "grace what did everyone just say")
	drain(1)
	for _, marker := range []string{"CAPTURE_IDLE_ALICE", "CAPTURE_IDLE_BOB", "CAPTURE_IDLE_DISCORD"} {
		if !strings.Contains(requests[0], marker) {
			t.Fatalf("first permitted request lost idle observation %q", marker)
		}
	}
	player("Bob", "CAPTURE_OTHER_DURING_ACTIVE")
	drain(1)
	decision = "continue"
	player("Alice", "and what about the latest message")
	drain(2)
	if !strings.Contains(requests[1], "CAPTURE_OTHER_DURING_ACTIVE") {
		t.Fatal("active follow-up missed another speaker's captured message")
	}
	decision = "disengage"
	player("Alice", "goodbye")
	drain(2) // Explicit endings are handled locally, without another model call.
	player("Alice", "CAPTURE_AFTER_DISENGAGE")
	drain(2)
	decision = "continue"
	player("Alice", "grace back for a moment")
	drain(3)
	if !strings.Contains(requests[2], "CAPTURE_AFTER_DISENGAGE") {
		t.Fatal("reopened conversation lost chat captured after disengagement")
	}
	if _, _, ready := a.engagements.take(time.Now().Add(2 * time.Minute)); ready {
		t.Fatal("silence expiry produced new model work")
	}
	player("Alice", "CAPTURE_AFTER_TIMEOUT")
	drain(3)
	discord("Carol", "grace what happened while we were quiet")
	drain(4)
	if !strings.Contains(requests[3], "CAPTURE_AFTER_TIMEOUT") {
		t.Fatal("new bridge conversation missed idle Minecraft context")
	}
	decision = "disengage"
	discord("Carol", "and the rest")
	drain(5)
	discord("Third Nick", "CAPTURE_BRIDGE_AFTER_DISENGAGE")
	drain(5)
	if entries := historyEntries(t, r.journal, "Alice", `{"query":"CAPTURE_BRIDGE_AFTER_DISENGAGE"}`); len(entries) != 1 {
		t.Fatal("disengaged bridge stopped capturing public messages")
	}
}
