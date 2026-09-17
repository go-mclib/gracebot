package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"
)

func localGameFixture(t *testing.T) (*chatRouter, *gameChat, context.Context, <-chan string) {
	t.Helper()
	r, m := moderationFixture(t)
	replies := make(chan string, 64)
	g := newGameChat(newGameManager(r.store), func(_ context.Context, text string) error { replies <- text; return nil }, func(actor string) bool { return m.blocked(actor, time.Now()) })
	r.games = g
	r.assistant.tools = newToolRegistry(g.tools())
	r.assistant.definitions = append(append([]openai.Tool(nil), r.assistant.tools.definitions...), responseTool)
	m.onChange = func(actor, by string) { r.assistant.moderationChanged(actor, by); g.forget(actor) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); g.run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return r, g, ctx, replies
}
func nextGameReply(t *testing.T, replies <-chan string) string {
	t.Helper()
	select {
	case text := <-replies:
		return text
	case <-time.After(2 * time.Second):
		t.Fatal("missing deterministic game reply")
		return ""
	}
}
func localGameControl(t *testing.T, g *gameChat, ctx context.Context, action, actor, raw string) gameResult {
	t.Helper()
	for _, spec := range g.tools() {
		if spec.name == action {
			value, err := spec.run(ctx, actor, json.RawMessage(raw))
			if err != nil {
				t.Fatal(err)
			}
			return value.(gameResult)
		}
	}
	t.Fatalf("missing game control %s", action)
	return gameResult{}
}

func TestLocalGameAnagramAnyoneCanGuessWithoutJoining(t *testing.T) {
	for _, source := range []string{"minecraft", "discord"} {
		t.Run(source, func(t *testing.T) {
			r, g, ctx, replies := localGameFixture(t)
			localGameControl(t, g, ctx, "start_game", "Alice", `{"game":"anagram","word":"later"}`)
			nextGameReply(t, replies)
			say := func(actor, text string) {
				if source == "discord" {
					r.discordChat(ctx, "GraceLocal", "[Discord] "+actor+" » "+text, time.Now())
				} else {
					r.playerChat(ctx, "GraceLocal", actor, text)
				}
			}
			say("Bob", "grape")
			nextGameReply(t, replies)
			say("Bob", "stop game")
			nextGameReply(t, replies)
			value, err := g.manager.status(ctx, "Alice", json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			state := value.(gameResult)
			if state.Status != "active" || state.AttemptsUsed != 1 {
				t.Fatalf("new participant could not guess, or acquired stop authority: %+v", state)
			}
			// A fresh player can win without a prefix. A real answer also takes
			// precedence over conversation-ending heuristics such as \"later\".
			say("Cara", "later")
			if text := nextGameReply(t, replies); !strings.Contains(text, "anagram won") || !strings.Contains(text, "answer later") {
				t.Fatalf("fresh participant's bare answer was not accepted: %s", text)
			}
			if _, _, ready := r.assistant.engagements.take(time.Now().Add(3 * time.Second)); ready {
				t.Fatal("anagram guesses fell through to the model")
			}
		})
	}
}

func TestLocalGameGuessesNeverCallOrUseModelNarration(t *testing.T) {
	r, g, ctx, replies := localGameFixture(t)
	calls := 0
	r.assistant.client = completionFunc(func(context.Context, completionRequest) (openai.ChatCompletionResponse, error) {
		calls++
		return openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{
			Role: "assistant", Content: "fabricated game clue", ToolCalls: []openai.ToolCall{{ID: "start", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "start_game", Arguments: `{"game":"wordle","word":"music"}`}}},
		}}}}, nil
	})
	r.assistant.send = func(context.Context, string) error { t.Error("model narrated a deterministic game"); return nil }
	r.playerChat(ctx, "GraceLocal", "Alice", "grace start wordle with custom word music")
	request, _, ready := r.assistant.engagements.take(time.Now().Add(3 * time.Second))
	if !ready {
		t.Fatal("natural start was not routed to tool-capable model")
	}
	r.assistant.handleRequest(ctx, request)
	if calls != 1 {
		t.Fatalf("start required %d model calls instead of one", calls)
	}
	if text := nextGameReply(t, replies); strings.Contains(text, "music") || strings.Contains(text, "fabricated") {
		t.Fatalf("start leaked/invented answer: %s", text)
	}
	r.playerChat(ctx, "GraceLocal", "Alice", "@GraceLocal jesus")
	if text := nextGameReply(t, replies); !strings.Contains(text, "s=") || !strings.Contains(text, "u~") || !strings.Contains(text, "s-") {
		t.Fatalf("wrong repeated-letter feedback: %s", text)
	}
	r.playerChat(ctx, "GraceLocal", "Bob", "water") // Unrelated five-letter ambient word.
	r.playerChat(ctx, "GraceLocal", "Alice", "acorn")
	if text := nextGameReply(t, replies); !strings.Contains(text, "c~") || strings.Contains(text, "s=") {
		t.Fatalf("latest guess was reinterpreted: %s", text)
	}
	state, err := g.manager.status(ctx, "Alice", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := state.(gameResult).AttemptsUsed; got != 2 {
		t.Fatalf("ambient chat consumed an attempt: %d", got)
	}
	r.playerChat(ctx, "GraceLocal", "Alice", "music")
	if text := nextGameReply(t, replies); !strings.Contains(text, "won") || !strings.Contains(text, "music") {
		t.Fatalf("missing deterministic win: %s", text)
	}
	if _, _, ready := r.assistant.engagements.take(time.Now().Add(3 * time.Second)); ready {
		t.Fatal("game input queued a model turn")
	}
	if calls != 1 {
		t.Fatalf("guesses made extra model calls: %d", calls)
	}
	if entries := historyEntries(t, r.journal, "Alice", `{"query":"acorn"}`); len(entries) != 1 {
		t.Fatal("locally consumed guess was not captured")
	}
}

func TestLocalGameBridgeParticipationDoesNotCaptureOtherDisplays(t *testing.T) {
	r, g, ctx, replies := localGameFixture(t)
	startCtx, scope := scopedTurn(ctx, discordBridgeActor)
	scope.display = "One"
	localGameControl(t, g, startCtx, "start_game", discordBridgeActor, `{"game":"wordle","word":"music"}`)
	nextGameReply(t, replies)
	r.discordChat(ctx, "GraceLocal", "[Discord] Two » water", time.Now())
	r.discordChat(ctx, "GraceLocal", "[Discord] One » jesus", time.Now())
	nextGameReply(t, replies)
	r.discordChat(ctx, "GraceLocal", "[Discord] Two » guess acorn", time.Now())
	nextGameReply(t, replies)
	state, err := g.manager.status(ctx, discordBridgeActor, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := state.(gameResult).AttemptsUsed; got != 2 {
		t.Fatalf("unfocused display changed the game: %d attempts", got)
	}
	r.discordChat(ctx, "GraceLocal", "[Discord] Two » lucky", time.Now())
	nextGameReply(t, replies)
	if _, _, ready := r.assistant.engagements.take(time.Now().Add(3 * time.Second)); ready {
		t.Fatal("bridge guess fell through to model")
	}
}

func TestLocalGameQueuedGuessCannotCrossDisconnect(t *testing.T) {
	manager := newGameManager(nil)
	var replies []string
	g := newGameChat(manager, func(_ context.Context, text string) error { replies = append(replies, text); return nil }, nil)
	ctx := context.Background()
	g.deliver(ctx, gameInput{ctx: ctx, actor: "Alice", action: "start_game", raw: json.RawMessage(`{"game":"hangman","word":"music"}`), session: g.session})
	if !g.route(ctx, "Alice", "", "guess music", "GraceLocal", nil) {
		t.Fatal("winning guess not consumed")
	}
	old := <-g.inputs
	g.reset()
	g.deliver(ctx, gameInput{ctx: ctx, actor: "Alice", action: "start_game", raw: json.RawMessage(`{"game":"hangman","word":"music"}`), session: g.session})
	g.deliver(ctx, old)
	state, err := manager.status(ctx, "Alice", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if state.(gameResult).GuessCount != 0 || state.(gameResult).Status != "active" || len(replies) != 2 {
		t.Fatal("old session guess changed or announced the replacement game")
	}
}

func TestLocalGameAddressingAndBlockedInputsStayOut(t *testing.T) {
	r, g, ctx, replies := localGameFixture(t)
	localGameControl(t, g, ctx, "start_game", "Alice", `{"game":"hangman","word":"music"}`)
	nextGameReply(t, replies)
	for _, text := range []string{"gracetoday", "hey guess music", "Bob, grace guess music"} {
		if g.route(ctx, "Other", "", text, "GraceLocal", []string{"grace"}) {
			t.Fatalf("ambient addressing became game input: %q", text)
		}
	}
	if _, err := r.moderation.block(ctx, "RealAdmin", "BlockedOne", "minecraft", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	r.playerChat(ctx, "GraceLocal", "BlockedOne", "guess music")
	r.playerChat(ctx, "GraceLocal", "Alice", "guess s")
	nextGameReply(t, replies)
	state, err := g.manager.status(ctx, "Alice", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if state.(gameResult).GuessCount != 1 || state.(gameResult).Progress != "__s__" {
		t.Fatal("blocked input affected the game")
	}
}
