package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func fragmentRequest(ctx context.Context, actor, text string, at time.Time, mention bool, id uint64) chatRequest {
	return chatRequest{ctx: ctx, sender: actor, username: "GraceLocal", mentioned: mention,
		messages: []chatFragment{{ID: id, At: at, Text: text}}}
}

func TestDebounceBatchesFragmentsAfterTwoSecondsOfQuiet(t *testing.T) {
	e := newConversationEngagement(time.Minute)
	defer e.reset()
	at := time.Now()
	e.enqueue(fragmentRequest(context.Background(), "Alice", "hey grace could you", at, true, 1))
	if _, _, ready := e.take(at.Add(1999 * time.Millisecond)); ready {
		t.Fatal("responded before the quiet period")
	}
	e.enqueue(fragmentRequest(context.Background(), "ALICE", "not do that", at.Add(time.Second), false, 2))
	if _, _, ready := e.take(at.Add(2 * time.Second)); ready {
		t.Fatal("new input did not restart debounce")
	}
	r, _, ready := e.take(at.Add(3 * time.Second))
	if !ready || len(r.messages) != 2 || r.messages[0].Text != "hey grace could you" || r.messages[1].ID != 2 {
		t.Fatalf("lost or split the speaker's batch: %+v", r)
	}
	e.finish(r, "wait")
	if _, _, ready := e.take(at.Add(20 * time.Second)); ready {
		t.Fatal("wait caused unsolicited polling or a reply")
	}
}

func TestWaitingAcceptsContinuationButIncomingSilenceExpires(t *testing.T) {
	e := newConversationEngagement(time.Minute)
	defer e.reset()
	at := time.Now()
	if e.enqueue(fragmentRequest(context.Background(), "Alice", "ambient", at, false, 1)) {
		t.Fatal("ambient chat engaged the bot")
	}
	e.enqueue(fragmentRequest(context.Background(), "Alice", "grace could you like", at, true, 2))
	r, _, _ := e.take(at.Add(2 * time.Second))
	e.finish(r, "wait")
	if !e.enqueue(fragmentRequest(context.Background(), "Alice", "not do that", at.Add(12*time.Second), false, 3)) {
		t.Fatal("a pause discarded unfinished conversation")
	}
	r, _, _ = e.take(at.Add(14 * time.Second))
	e.finish(r, "continue")
	if e.enqueue(fragmentRequest(context.Background(), "Bob", "yes", at.Add(20*time.Second), false, 4)) {
		t.Fatal("engagement leaked to another speaker")
	}
	if e.enqueue(fragmentRequest(context.Background(), "Alice", "yes", at.Add(72*time.Second), false, 5)) {
		t.Fatal("silence boundary was refreshed by the bot's response")
	}
	if !e.enqueue(fragmentRequest(context.Background(), "Alice", "grace again", at.Add(73*time.Second), true, 6)) {
		t.Fatal("new mention did not reopen conversation")
	}
}

func TestNewInputCancelsObsoleteToolsAndStaleDisengagement(t *testing.T) {
	e := newConversationEngagement(time.Minute)
	defer e.reset()
	at := time.Now()
	e.enqueue(fragmentRequest(context.Background(), "Alice", "grace start", at, true, 1))
	old, _, _ := e.take(at.Add(2 * time.Second))
	e.enqueue(fragmentRequest(context.Background(), "Alice", "actually stop", at.Add(3*time.Second), false, 2))
	if old.ctx.Err() != context.Canceled {
		t.Fatal("new input left the old turn able to publish or perform effects")
	}
	e.finish(old, "disengage")
	fresh, _, ready := e.take(at.Add(5 * time.Second))
	if !ready || fresh.messages[0].Text != "actually stop" {
		t.Fatal("stale decision ended the new request")
	}
	e.finish(fresh, "disengage")
	if e.enqueue(fragmentRequest(context.Background(), "Alice", "ambient", at.Add(6*time.Second), false, 3)) {
		t.Fatal("disengaged conversation reactivated without a mention")
	}
}

func TestDisconnectedPendingInputNeverStarts(t *testing.T) {
	e := newConversationEngagement(time.Minute)
	defer e.reset()
	ctx, cancel := context.WithCancel(context.Background())
	at := time.Now()
	e.enqueue(fragmentRequest(ctx, "Alice", "grace do it", at, true, 1))
	cancel()
	if _, _, ready := e.take(at.Add(3 * time.Second)); ready {
		t.Fatal("disconnected request escaped into another session")
	}
}

func TestPendingFloodRetainsBoundedLatestEvents(t *testing.T) {
	e := newConversationEngagement(time.Minute)
	defer e.reset()
	at := time.Now()
	for i := 0; i < maxPendingMessages+10; i++ {
		e.enqueue(fragmentRequest(context.Background(), "Alice", fmt.Sprint(i), at.Add(time.Duration(i)*time.Millisecond), i == 0, uint64(i+1)))
	}
	r, _, ready := e.take(at.Add(3 * time.Second))
	if !ready || len(r.messages) != maxPendingMessages || r.omitted != 10 || r.messages[0].ID != 11 {
		t.Fatalf("flood was not bounded with explicit history coverage: %+v", r)
	}
}
