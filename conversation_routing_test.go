package main

import (
	"context"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"
)

func TestAudienceDiversionCancelsOnlyItsOwnersTurn(t *testing.T) {
	for _, text := range []string{"@Bob can you help", "Bob: help please", "Bob, are you selling iron", "hey Bob need iron", "Bob are you selling iron"} {
		t.Run(text, func(t *testing.T) {
			e := newConversationEngagement(time.Minute)
			defer e.reset()
			at := time.Now()
			e.observe("Bob", "", "", at)
			e.enqueue(fragmentRequest(context.Background(), "Alice", "grace help", at, true, 1))
			a, _, _ := e.take(at.Add(2 * time.Second))
			e.enqueue(fragmentRequest(context.Background(), "Charlie", "grace help", at, true, 2))
			c, _, _ := e.take(at.Add(2 * time.Second))
			e.observe("Alice", "", text, at.Add(3*time.Second))
			if e.enqueue(fragmentRequest(context.Background(), "Alice", text, at.Add(3*time.Second), false, 3)) {
				t.Fatal("direct address to another player queued model work")
			}
			if a.ctx.Err() != context.Canceled || c.ctx.Err() != nil {
				t.Fatalf("diversion cancellation crossed owners: Alice=%v Charlie=%v", a.ctx.Err(), c.ctx.Err())
			}
			if _, _, ready := e.take(at.Add(5 * time.Second)); ready {
				t.Fatal("diverted owner retained pending work")
			}
		})
	}
}

func TestTalkingAboutKnownPlayerPreservesUnfinishedInput(t *testing.T) {
	e := newConversationEngagement(time.Minute)
	defer e.reset()
	at := time.Now()
	e.observe("Bob", "", "", at)
	e.enqueue(fragmentRequest(context.Background(), "Alice", "grace could you", at, true, 1))
	for i, text := range []string{"help Bob build a house", "Bob is mining today", "the tag @Bobby is different"} {
		if !e.enqueue(fragmentRequest(context.Background(), "Alice", text, at.Add(time.Duration(i+1)*time.Millisecond), false, uint64(i+2))) {
			t.Fatalf("ordinary mention of a player was treated as addressing them: %q", text)
		}
	}
	r, _, ready := e.take(at.Add(3 * time.Second))
	if !ready || len(r.messages) != 4 {
		t.Fatal("unfinished request was split or lost")
	}
}

func TestAudienceDiversionDropsPendingBatchAndReopensFresh(t *testing.T) {
	e := newConversationEngagement(time.Minute)
	defer e.reset()
	at := time.Now()
	e.observe("Bob", "", "", at)
	e.enqueue(fragmentRequest(context.Background(), "Alice", "grace help", at, true, 1))
	if e.enqueue(fragmentRequest(context.Background(), "Alice", "Bob, help instead", at.Add(time.Second), false, 2)) {
		t.Fatal("diverted fragment queued")
	}
	if _, _, ready := e.take(at.Add(3 * time.Second)); ready {
		t.Fatal("obsolete pending batch reached worker")
	}
	e.enqueue(fragmentRequest(context.Background(), "Alice", "grace new request", at.Add(4*time.Second), true, 3))
	r, _, ready := e.take(at.Add(6 * time.Second))
	if !ready || len(r.messages) != 1 || r.messages[0].ID != 3 {
		t.Fatalf("new mention inherited obsolete fragments: %+v", r.messages)
	}
	e.finish(r, "wait")
	e.enqueue(fragmentRequest(context.Background(), "Alice", "and another thing", at.Add(8*time.Second), false, 4))
	followup, _, _ := e.take(at.Add(10 * time.Second))
	if r.conversationID == 0 || followup.conversationID != r.conversationID {
		t.Fatal("follow-up lost the engagement identity")
	}
	e.finish(followup, "disengage")
	e.enqueue(fragmentRequest(context.Background(), "Alice", "grace again", at.Add(12*time.Second), true, 5))
	reopened, _, ready := e.take(at.Add(14 * time.Second))
	if !ready || reopened.conversationID == 0 || reopened.conversationID == r.conversationID {
		t.Fatal("reopened engagement reused stale model context identity")
	}
}

func TestThirdPartyPromptDisambiguatesShortReplies(t *testing.T) {
	for _, question := range []string{"Alice, need iron?", "anyone selling iron", "do you need iron", "want iron?"} {
		t.Run(question, func(t *testing.T) {
			e := newConversationEngagement(time.Minute)
			defer e.reset()
			at := time.Now()
			e.observe("Alice", "", "", at)
			e.enqueue(fragmentRequest(context.Background(), "Alice", "grace help", at, true, 1))
			r, _, _ := e.take(at.Add(2 * time.Second))
			e.finish(r, "wait")
			e.observe("Bob", "", question, at.Add(3*time.Second))
			e.observe("Alice", "", "yes", at.Add(4*time.Second))
			if e.enqueue(fragmentRequest(context.Background(), "Alice", "yes", at.Add(4*time.Second), false, 2)) {
				t.Fatal("ambiguous third-party answer reached the bot")
			}
			if !e.enqueue(fragmentRequest(context.Background(), "Alice", "grace yes", at.Add(5*time.Second), true, 3)) {
				t.Fatal("explicit mention could not restore focus")
			}
		})
	}
}

func TestBridgeDisplayFocusDoesNotSplitActorOrCancelOtherNickname(t *testing.T) {
	e := newConversationEngagement(time.Minute)
	defer e.reset()
	at := time.Now()
	request := func(display, text string, seconds int, mention bool) chatRequest {
		r := fragmentRequest(context.Background(), discordBridgeActor, text, at.Add(time.Duration(seconds)*time.Second), mention, uint64(seconds+1))
		r.messages[0].DisplayName = display
		e.observe(discordBridgeActor, display, text, r.messages[0].At)
		return r
	}
	e.enqueue(request("Alice", "grace help", 0, true))
	first, _, _ := e.take(at.Add(2 * time.Second))
	if e.enqueue(request("Bob", "actually stop", 3, false)) || first.ctx.Err() != nil {
		t.Fatal("unfocused nickname joined or cancelled bridge conversation")
	}
	if !e.enqueue(request("ALICE", "and what about that?", 4, false)) {
		t.Fatal("normalized focused nickname lost continuation")
	}
	if first.ctx.Err() != context.Canceled {
		t.Fatal("focused participant did not cancel obsolete bridge turn")
	}
	if !e.enqueue(request("Bob", "grace I can help too", 5, true)) {
		t.Fatal("explicit mention did not allow another nickname to participate")
	}
	batch, _, ready := e.take(at.Add(7 * time.Second))
	if !ready || batch.sender != discordBridgeActor || batch.conversationID != first.conversationID || len(batch.messages) != 2 {
		t.Fatal("explicit bridge participants split actor/context or lost labelled fragments")
	}
	if e.enqueue(request("Alice", "yes", 8, false)) || batch.ctx.Err() != nil {
		t.Fatal("previous nickname kept automatic focus after explicit handoff")
	}
	e.finish(batch, "wait")
	e.observe(discordBridgeActor, "Alice", "Bob, need iron?", at.Add(9*time.Second))
	if e.enqueue(request("Bob", "yes", 10, false)) {
		t.Fatal("another bridge nickname's question was mistaken for the bot's")
	}
}

func TestCompletedFollowupsAndConfiguredTimeoutBounds(t *testing.T) {
	for _, decision := range []string{"continue", "wait"} {
		t.Run(decision, func(t *testing.T) {
			e := newConversationEngagement(time.Minute)
			defer e.reset()
			at := time.Now()
			e.enqueue(fragmentRequest(context.Background(), "Alice", "grace help", at, true, 1))
			r, _, _ := e.take(at.Add(2 * time.Second))
			e.finish(r, decision)
			accepted := e.enqueue(fragmentRequest(context.Background(), "Alice", "I am going mining now", at.Add(4*time.Second), false, 2))
			if accepted != (decision == "wait") {
				t.Fatalf("completed versus incomplete exchange routing = %v", accepted)
			}
		})
	}
	for _, timeout := range []time.Duration{0, 5 * time.Second, time.Minute} {
		t.Run(timeout.String(), func(t *testing.T) {
			e := newConversationEngagement(timeout)
			defer e.reset()
			at := time.Now()
			e.enqueue(fragmentRequest(context.Background(), "Alice", "grace help", at, true, 1))
			r, _, _ := e.take(at.Add(2 * time.Second))
			e.finish(r, "continue")
			boundary := completedFollowupWindow
			if timeout < boundary {
				boundary = timeout
			}
			if e.enqueue(fragmentRequest(context.Background(), "Alice", "why?", at.Add(boundary), false, 2)) {
				t.Fatal("follow-up exceeded configured or completed-answer maximum")
			}
			if !e.enqueue(fragmentRequest(context.Background(), "Alice", "grace why?", at.Add(boundary+time.Second), true, 3)) {
				t.Fatal("timeout prevented an explicit mention")
			}
		})
	}
}

func TestConversationEndingsCancelWithoutAnotherTurn(t *testing.T) {
	for _, text := range []string{"thanks!", "never mind", "leave me alone", "that's all"} {
		t.Run(text, func(t *testing.T) {
			e := newConversationEngagement(time.Minute)
			defer e.reset()
			at := time.Now()
			e.enqueue(fragmentRequest(context.Background(), "Alice", "grace help", at, true, 1))
			r, _, _ := e.take(at.Add(2 * time.Second))
			if e.enqueue(fragmentRequest(context.Background(), "Alice", text, at.Add(3*time.Second), false, 2)) || r.ctx.Err() != context.Canceled {
				t.Fatal("ending kept obsolete work or asked the model to say goodbye")
			}
		})
	}
}

func TestAudienceRoutingAPICounter(t *testing.T) {
	calls := 0
	decision := "continue"
	a := newAssistant(completionFunc(func(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
		calls++
		if decision == "continue" {
			return responseCompletion(decision, "sure"), nil
		}
		return responseCompletion(decision), nil
	}), "model", "", newToolRegistry(), func(context.Context, string) error { return nil })
	defer a.engagements.reset()
	at := time.Now()
	a.engagements.observe("Bob", "", "", at)
	step := func(actor, text string, seconds, wantCalls int, mention bool) {
		t.Helper()
		now := at.Add(time.Duration(seconds) * time.Second)
		a.engagements.observe(actor, "", text, now)
		a.engagements.enqueue(fragmentRequest(context.Background(), actor, text, now, mention, uint64(seconds+1)))
		if r, _, ready := a.engagements.take(now.Add(2 * time.Second)); ready {
			a.handleRequest(context.Background(), r)
		}
		if calls != wantCalls {
			t.Fatalf("%s: %q => API calls %d, want %d", actor, text, calls, wantCalls)
		}
	}
	step("Alice", "grace help", 0, 1, true)
	step("Alice", "how much iron?", 3, 2, false)
	step("Alice", "Bob, need iron?", 6, 2, false)
	decision = "wait"
	step("Alice", "grace could you", 9, 3, true)
	decision = "continue"
	step("Alice", "build a small house near spawn", 12, 4, false)
	step("Bob", "Alice, do you need iron?", 15, 4, false)
	step("Alice", "yes", 18, 4, false)
	step("Alice", "grace and what about that?", 21, 5, true)
	step("Alice", "thanks", 24, 5, false)
	step("Alice", "grace again", 27, 6, true)
	step("Alice", "I am going mining now", 30, 6, false)
}

func TestSubstantiveLinkedFollowupsAreNotWordLimited(t *testing.T) {
	e := newConversationEngagement(time.Minute)
	defer e.reset()
	at := time.Now()
	e.enqueue(fragmentRequest(context.Background(), "Alice", "grace help", at, true, 1))
	r, _, _ := e.take(at.Add(2 * time.Second))
	e.finish(r, "continue")
	text := "also can you explain how the different blocks fit together and where the windows and doors should go in the finished house"
	if !e.enqueue(fragmentRequest(context.Background(), "Alice", text, at.Add(4*time.Second), false, 2)) {
		t.Fatal("substantive linked follow-up was rejected for length")
	}
	r, _, _ = e.take(at.Add(6 * time.Second))
	e.finish(r, "continue")
	text = "can you move a little farther east past the river and build a bridge between the two hills near spawn"
	if !e.enqueue(fragmentRequest(context.Background(), "Alice", text, at.Add(8*time.Second), false, 3)) {
		t.Fatal("direct follow-up was rejected for length")
	}
}

func TestPublicDisplayCannotDivertConfiguredBotAlias(t *testing.T) {
	e := newConversationEngagement(time.Minute)
	defer e.reset()
	at := time.Now()
	e.setBotNames("GraceLocal", []string{"grace", "helper"})
	e.observe(discordBridgeActor, "Grace", "", at)
	e.observe("Bob", "", "", at)
	if !e.enqueue(fragmentRequest(context.Background(), "Alice", "hey grace help me", at, true, 1)) {
		t.Fatal("public display label shadowed the configured bot alias")
	}
	r, _, _ := e.take(at.Add(2 * time.Second))
	if e.enqueue(fragmentRequest(context.Background(), "Alice", "Bob, grace said to ask you", at.Add(3*time.Second), true, 2)) || r.ctx.Err() != context.Canceled {
		t.Fatal("mentioning the bot while addressing another player kept obsolete work")
	}
}
