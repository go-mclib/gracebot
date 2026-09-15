package main

import (
	"context"
	"strings"
	"sync"
	"time"
)

const (
	defaultEngagementTimeout = time.Minute
	defaultChatDebounce      = 2 * time.Second
	maxPendingMessages       = 32
	consoleActor             = "@console"
)

type engagement struct {
	revision       uint64
	conversationID uint64
	lastInput      time.Time
	completed      bool
	focus          string
	until, readyAt time.Time
	request        chatRequest
	cancel         context.CancelFunc
}

// Chat handlers persist eligible input before this gate. Only a mention or a
// live conversation queues a model turn; idle chat stays local. A single worker
// takes quiet batches; new input cancels an obsolete turn before its remaining
// tools or messages can execute.
// Expiry is measured from incoming messages, never from a reply or tool result.
type conversationEngagement struct {
	mu                sync.Mutex
	timeout, debounce time.Duration
	next              uint64
	nextConversation  uint64
	names             map[string]time.Time
	botNames          map[string]bool
	prompts           []routingPrompt
	players           map[string]*engagement
	wake              chan struct{}
}

func newConversationEngagement(timeout time.Duration) *conversationEngagement {
	return &conversationEngagement{timeout: timeout, debounce: defaultChatDebounce,
		players: make(map[string]*engagement), names: make(map[string]time.Time), wake: make(chan struct{}, 1)}
}

func (e *conversationEngagement) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *conversationEngagement) enqueue(request chatRequest) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(request.messages) == 0 {
		return false
	}
	key := strings.ToLower(request.sender)
	last := request.messages[len(request.messages)-1]
	at := last.At
	state := e.players[key]
	if state != nil && (!at.Before(state.until) || state.request.ctx.Err() != nil) {
		if state.cancel != nil {
			state.cancel()
		}
		delete(e.players, key)
		state = nil
	}
	// An unmentioned bridge nickname is not the focused participant, but it
	// remains the same actor for storage, privileges and explicit mentions.
	if state != nil && isDiscordActor(request.sender) && routingName(last.DisplayName) != state.focus && !request.mentioned {
		return false
	}
	diverted := false
	for _, fragment := range request.messages {
		recipient := addressedPlayer(fragment.Text, e.names)
		if recipient != "" && !strings.EqualFold(recipient, request.username) && !e.botNames[recipient] {
			diverted = true
		}
	}
	if diverted || !request.mentioned && (endsConversation(last.Text) || state != nil && e.competingPrompt(state, last.Text, at)) {
		if state != nil {
			e.closeLocked(key)
		}
		return false
	}
	if !request.mentioned && (state == nil || e.timeout == 0) {
		return false
	}
	if state != nil && !request.mentioned && state.completed && !contextualFollowup(last.Text) {
		e.closeLocked(key)
		return false
	}
	if state == nil {
		if len(e.players) >= maxConversations {
			var oldest string
			for name, entry := range e.players {
				if oldest == "" || entry.until.Before(e.players[oldest].until) {
					oldest = name
				}
			}
			if old := e.players[oldest]; old.cancel != nil {
				old.cancel()
			}
			delete(e.players, oldest)
		}
		e.nextConversation++
		state = &engagement{conversationID: e.nextConversation}
		e.players[key] = state
	}
	if state.cancel != nil {
		state.cancel()
		state.cancel = nil
	}
	pending := state.request.messages
	omitted := state.request.omitted
	mention := state.request.mentioned
	for _, fragment := range request.messages {
		if len(pending) == maxPendingMessages {
			copy(pending, pending[1:])
			pending = pending[:len(pending)-1]
			omitted++
		}
		pending = append(pending, fragment)
	}
	e.next++
	state.revision = e.next
	state.until = at.Add(e.timeout)
	state.lastInput = at
	state.completed = false
	state.focus = routingName(last.DisplayName)
	if e.timeout == 0 {
		state.until = at.Add(defaultEngagementTimeout)
	}
	state.readyAt = at.Add(e.debounce)
	state.request = request
	state.request.messages = pending
	state.request.omitted = omitted
	state.request.mentioned = mention || request.mentioned
	state.request.revision = state.revision
	state.request.conversationID = state.conversationID
	e.signal()
	return true
}

// take returns either a due batch or the next time at which work/expiry is due.
// Returned input is owned by the worker, not by a subsequently arriving batch.
func (e *conversationEngagement) take(now time.Time) (chatRequest, time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var chosen *engagement
	var next time.Time
	for name, state := range e.players {
		if !now.Before(state.until) || state.request.ctx.Err() != nil {
			if state.cancel != nil {
				state.cancel()
			}
			delete(e.players, name)
			continue
		}
		when := state.until
		if len(state.request.messages) != 0 {
			when = minTime(when, state.readyAt)
		}
		if next.IsZero() || when.Before(next) {
			next = when
		}
		if len(state.request.messages) != 0 && !now.Before(state.readyAt) &&
			(chosen == nil || state.readyAt.Before(chosen.readyAt) || (state.readyAt.Equal(chosen.readyAt) && state.revision < chosen.revision)) {
			chosen = state
		}
	}
	if chosen == nil {
		return chatRequest{}, next, false
	}
	request := chosen.request
	request.ctx, chosen.cancel = context.WithDeadline(request.ctx, chosen.until)
	chosen.request.messages = nil
	chosen.request.omitted = 0
	chosen.request.mentioned = false
	return request, next, true
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (e *conversationEngagement) finish(request chatRequest, decision string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := strings.ToLower(request.sender)
	if state := e.players[key]; state != nil && state.revision == request.revision {
		if state.cancel != nil {
			state.cancel()
			state.cancel = nil
		}
		if decision == "disengage" {
			delete(e.players, key)
		} else if decision != "wait" {
			// Responses never refresh engagement. A completed answer gets
			// only a short contextual follow-up window, even if the model
			// accidentally leaves the conversation open.
			state.completed = true
			state.until = minTime(state.until, state.lastInput.Add(completedFollowupWindow))
		}
	}
}

func (e *conversationEngagement) closeLocked(key string) {
	if state := e.players[key]; state != nil {
		if state.cancel != nil {
			state.cancel()
		}
		delete(e.players, key)
		e.signal()
	}
}

// forget immediately cancels work and drops queued input for a blocked actor.
func (e *conversationEngagement) forget(actor string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := normalizeStoreName(actor)
	if state := e.players[key]; state != nil {
		if state.cancel != nil {
			state.cancel()
		}
		delete(e.players, key)
	}
	e.signal()
}

func (e *conversationEngagement) reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, state := range e.players {
		if state.cancel != nil {
			state.cancel()
		}
	}
	clear(e.players)
	clear(e.names)
	e.prompts = nil
	e.signal()
}
