package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"
)

func moderationFixture(t *testing.T) (*chatRouter, *chatModeration) {
	t.Helper()
	store := testPlayerStore(t)
	m := newChatModeration(store, nil)
	a := newAssistant(nil, "model", "", newToolRegistry(m.tools()), nil)
	a.journal, a.moderation = store.history(), m
	m.onChange = a.moderationChanged
	t.Cleanup(a.engagements.reset)
	return &chatRouter{assistant: a, store: store, journal: a.journal, aliases: []string{"grace"}, moderation: m}, m
}

func TestBlockDurationsNeverTurnInvalidInputPermanent(t *testing.T) {
	at := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	for raw, want := range map[string]time.Duration{"1 hour 30 minutes": 90 * time.Minute, "2d": 48 * time.Hour, "1h30m": 90 * time.Minute, "3 weeks 2 days": 23 * 24 * time.Hour} {
		until, err := parseBlockDuration(raw, at)
		if err != nil || until == nil || until.Sub(at) != want {
			t.Fatalf("%q: %v %v", raw, until, err)
		}
	}
	for _, raw := range []string{"", "forever", "permanent"} {
		until, err := parseBlockDuration(raw, at)
		if err != nil || until != nil {
			t.Fatalf("permanent %q: %v %v", raw, until, err)
		}
	}
	for _, raw := range []string{"0 minutes", "-2h", "1 month", "1.5h", "12", "1h garbage", "999999999999999999999999 weeks"} {
		if _, err := parseBlockDuration(raw, at); err == nil {
			t.Fatalf("invalid duration %q accepted", raw)
		}
	}
}

func TestAdminCommandsDiscardBlockedChatAndCancelPendingWork(t *testing.T) {
	r, m := moderationFixture(t)
	ctx := context.Background()
	r.playerChat(ctx, "GraceLocal", "Alice", "grace pending before block")
	r.playerChat(ctx, "GraceLocal", "RealAdmin", "!block Alice for 1 hour 30 minutes")
	blocks, err := m.store.blockedActors(ctx, time.Now())
	if err != nil || len(blocks) != 1 || blocks[0].Actor != "alice" || blocks[0].ExpiresAt == nil {
		t.Fatalf("command block: %+v %v", blocks, err)
	}
	if remaining := time.Until(*blocks[0].ExpiresAt); remaining < 89*time.Minute || remaining > 91*time.Minute {
		t.Fatalf("duration=%v", remaining)
	}
	r.playerChat(ctx, "GraceLocal", "Alice", "grace BLOCKED_MESSAGE_SECRET")
	if _, _, ready := r.assistant.engagements.take(time.Now().Add(3 * time.Second)); ready {
		t.Fatal("blocked/queued speaker reached engagement")
	}
	var count int
	if err := m.store.db.QueryRow("SELECT count(*) FROM events WHERE text LIKE '%BLOCKED_MESSAGE_SECRET%'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("blocked message persisted: %d %v", count, err)
	}
	if context := r.journal.recentContext(0, "RealAdmin"); strings.Contains(context, "pending before block") {
		t.Fatalf("pre-block history leaked: %s", context)
	}
	r.playerChat(ctx, "GraceLocal", "RealAdmin", "!unblock Alice")
	r.playerChat(ctx, "GraceLocal", "Alice", "grace visible after unblock")
	request, _, ready := r.assistant.engagements.take(time.Now().Add(3 * time.Second))
	if !ready || request.sender != "Alice" || len(request.messages) != 1 || request.messages[0].Text != "grace visible after unblock" {
		t.Fatalf("unblock did not restore fresh engagement: %+v", request)
	}
}

func TestDiscordNicknameBlockCoversRenamesWithoutMinecraftCollision(t *testing.T) {
	r, m := moderationFixture(t)
	ctx, at := context.Background(), time.Now()
	r.discordChat(ctx, "GraceLocal", "[Discord] Alice » grace bridge mention", at)
	r.playerChat(ctx, "GraceLocal", "Alice", "ordinary Minecraft conversation")
	if _, err := m.block(ctx, "RealAdmin", "Alice", "", "", at); err == nil {
		t.Fatal("ambiguous Discord/Minecraft target guessed a platform")
	}
	if _, err := m.block(ctx, "RealAdmin", "Alice", "discord", "", at); err != nil {
		t.Fatal(err)
	}
	for _, frame := range []string{"[Discord] Renamed » grace BRIDGE_BLOCKED_SECRET", "[Discord] RealAdmin » grace BRIDGE_BLOCKED_SECRET", "[Discord] malformed BRIDGE_BLOCKED_SECRET"} {
		if !r.discordChat(ctx, "GraceLocal", frame, at.Add(time.Second)) {
			t.Fatal("blocked bridge frame fell into anonymous system history")
		}
	}
	r.playerChat(ctx, "GraceLocal", "Alice", "grace Minecraft remains visible")
	request, _, ready := r.assistant.engagements.take(at.Add(3 * time.Second))
	if !ready || request.sender != "Alice" {
		t.Fatalf("bridge collided with Minecraft: %+v", request)
	}
	var count int
	if err := m.store.db.QueryRow("SELECT count(*) FROM events WHERE text LIKE '%BRIDGE_BLOCKED_SECRET%'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("blocked bridge saved: %d %v", count, err)
	}
	if _, err := m.unblock(ctx, "RealAdmin", "discord:Alice", "", at); err != nil {
		t.Fatal(err)
	}
	r.discordChat(ctx, "GraceLocal", "[Discord] New Name » grace restored", at.Add(4*time.Second))
	request, _, ready = r.assistant.engagements.take(at.Add(7 * time.Second))
	if !ready || request.sender != discordBridgeActor || request.messages[0].DisplayName != "New Name" {
		t.Fatalf("shared bridge not restored: %+v", request)
	}
}

func TestSpamTimeoutUsesRollingWindowsAndExactExpiry(t *testing.T) {
	for _, mode := range []string{"burst", "repeats"} {
		t.Run(mode, func(t *testing.T) {
			_, m := moderationFixture(t)
			ctx, at := context.Background(), time.Now()
			limit := spamBurstLimit
			if mode == "repeats" {
				limit = spamRepeatLimit
			}
			for i := 0; i < limit; i++ {
				text := fmt.Sprintf("message %d", i)
				if mode == "repeats" {
					text = "  SAME   repeated message "
				}
				accepted := m.admit(ctx, "Alice", text, at.Add(time.Duration(i)*time.Millisecond))
				if accepted != (i < limit-1) {
					t.Fatalf("message %d admitted=%v", i, accepted)
				}
			}
			blocks, err := m.store.blockedActors(ctx, at.Add(time.Second))
			if err != nil || len(blocks) != 1 || blocks[0].ExpiresAt == nil {
				t.Fatalf("spam block=%+v %v", blocks, err)
			}
			trigger := at.Add(time.Duration(limit-1) * time.Millisecond)
			if !blocks[0].ExpiresAt.Equal(trigger.Add(10 * time.Minute)) {
				t.Fatalf("timeout=%v", blocks[0].ExpiresAt)
			}
			if m.admit(ctx, "Alice", "ignored", blocks[0].ExpiresAt.Add(-time.Nanosecond)) {
				t.Fatal("timeout expired early")
			}
			if !m.admit(ctx, "Alice", "fresh after expiry", *blocks[0].ExpiresAt) {
				t.Fatal("timeout failed to expire")
			}
			if !m.admit(ctx, "Bob", "unrelated", at) {
				t.Fatal("timeout affected another Minecraft account")
			}
		})
	}
	_, m := moderationFixture(t)
	at := time.Now()
	for i := 0; i < 20; i++ {
		if !m.admit(context.Background(), "Alice", "same but slow", at.Add(time.Duration(i)*11*time.Second)) {
			t.Fatal("expired samples counted as spam")
		}
	}
}

func TestModerationToolsRejectUnprivilegedAndForgedAuthority(t *testing.T) {
	_, m := moderationFixture(t)
	for _, actor := range []string{"Alice", discordBridgeActor, "@discord:RealAdmin", "@server"} {
		for _, tool := range m.tools() {
			if _, err := tool.run(context.Background(), actor, json.RawMessage(`{"target":"Bob"}`)); err == nil {
				t.Fatalf("%s invoked %s", actor, tool.name)
			}
		}
	}
	if _, err := m.tools()[0].run(context.Background(), "Alice", json.RawMessage(`{"target":"Bob","actor":"RealAdmin"}`)); err == nil {
		t.Fatal("model-supplied identity was accepted")
	}
}

func TestBlockingDuringModelRequestCancelsOutputAndCachedContext(t *testing.T) {
	r, m := moderationFixture(t)
	started := make(chan struct{})
	done := make(chan struct{})
	r.assistant.client = completionFunc(func(ctx context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		close(started)
		<-ctx.Done()
		return responseCompletion("continue", "obsolete response"), nil // Deliberately late provider reply.
	})
	r.assistant.send = func(context.Context, string) error { t.Error("blocked in-flight response was published"); return nil }
	request := fragmentRequest(context.Background(), "Alice", "grace old request", time.Now(), true, 1)
	go func() { defer close(done); r.assistant.handleRequest(context.Background(), request) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("model never started")
	}
	if _, err := m.block(context.Background(), "RealAdmin", "Alice", "minecraft", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("blocked model call was not cancelled")
	}
	if conv := r.assistant.history["alice"]; conv != nil && len(conv.turns) != 0 {
		t.Fatal("blocked request retained in cached conversation")
	}
}

func TestAdminToolBlockRebuildsContextBeforeNextModelCall(t *testing.T) {
	r, m := moderationFixture(t)
	if _, err := r.journal.append("player", "Alice", "CONTEXT_POISON_MARKER", time.Now()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	r.assistant.client = completionFunc(func(_ context.Context, request openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		calls++
		data, _ := json.Marshal(request.Messages)
		if calls == 1 {
			if !strings.Contains(string(data), "CONTEXT_POISON_MARKER") {
				t.Fatal("fixture never loaded prior context")
			}
			return completionMessage(openai.ChatCompletionMessage{ToolCalls: []openai.ToolCall{{ID: "block", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "block_chat", Arguments: `{"target":"Alice","source":"minecraft"}`}}}}), nil
		}
		if strings.Contains(string(data), "CONTEXT_POISON_MARKER") {
			t.Fatal("new model request retained blocked background")
		}
		return responseCompletion("continue", "blocked"), nil
	})
	var replies []string
	r.assistant.send = func(_ context.Context, text string) error { replies = append(replies, text); return nil }
	r.assistant.handleRequest(context.Background(), fragmentRequest(context.Background(), "RealAdmin", "grace block Alice", time.Now(), true, 0))
	if calls != 2 || len(replies) != 1 || !m.blocked("Alice", time.Now()) {
		t.Fatalf("admin block flow: calls=%d replies=%v", calls, replies)
	}
}
