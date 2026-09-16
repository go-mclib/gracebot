package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
)

type gameParticipant struct{ actor, display string }
type gameInput struct {
	ctx                    context.Context
	actor, display, action string
	raw                    json.RawMessage
	session, round         uint64
	done                   chan gameDelivery
}
type gameDelivery struct {
	value any
	err   error
}

// Mutations and their factual replies share one worker. Packet handlers only
// classify/enqueue; they never wait for paced chat writes or the model.
type gameChat struct {
	mu             sync.Mutex
	manager        *gameManager
	send           func(context.Context, string) error
	blocked        func(string) bool
	inputs         chan gameInput
	handlers       map[string]func(context.Context, string, json.RawMessage) (any, error)
	specs          []toolSpec
	session, round uint64
	kind           string
	length         int
	participants   map[gameParticipant]bool
	runningActor   string
	runningCancel  context.CancelFunc
}

func newGameChat(manager *gameManager, send func(context.Context, string) error, blocked func(string) bool) *gameChat {
	g := &gameChat{manager: manager, send: send, blocked: blocked, inputs: make(chan gameInput, 32), handlers: make(map[string]func(context.Context, string, json.RawMessage) (any, error)), session: 1}
	for _, spec := range manager.tools() {
		g.handlers[spec.name] = spec.run
		g.specs = append(g.specs, spec)
	}
	g.handlers["guess"] = manager.guess
	return g
}
func (g *gameChat) tools() []toolSpec {
	result := make([]toolSpec, 0, len(g.specs))
	for _, spec := range g.specs {
		action := spec.name
		spec.endsTurn = true
		spec.run = func(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
			display := ""
			if scope := replyScope(ctx); scope != nil {
				display = scope.display
			}
			g.mu.Lock()
			event := gameInput{ctx: ctx, actor: actor, display: display, action: action, raw: raw, session: g.session, done: make(chan gameDelivery, 1)}
			g.mu.Unlock()
			select {
			case g.inputs <- event:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			select {
			case result := <-event.done:
				return result.value, result.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		result = append(result, spec)
	}
	return result
}
func (g *gameChat) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			g.reset()
			return
		case event := <-g.inputs:
			g.deliver(ctx, event)
		}
	}
}
func (g *gameChat) deliver(workerCtx context.Context, event gameInput) {
	result := gameDelivery{}
	defer func() {
		if event.done != nil {
			event.done <- result
		}
	}()
	ctx, cancel := context.WithCancel(event.ctx)
	defer cancel()
	stop := context.AfterFunc(workerCtx, cancel)
	defer stop()
	g.mu.Lock()
	if ctx.Err() != nil || workerCtx.Err() != nil || event.session != g.session || event.action == "guess" && event.round != g.round || g.isBlocked(event.actor) {
		g.mu.Unlock()
		result.err = context.Canceled
		return
	}
	g.runningActor, g.runningCancel = event.actor, cancel
	if scope := replyScope(ctx); event.action == "start_game" && scope != nil && scope.private.Load() && event.actor != consoleActor {
		result.err = fmt.Errorf("start a shared game in a fresh public request, not from private tool data")
	} else {
		result.value, result.err = g.handlers[event.action](ctx, event.actor, event.raw)
	}
	if result.err == nil {
		state := result.value.(gameResult)
		switch {
		case event.action == "start_game":
			g.round++
			g.kind, g.length = state.Game, state.Length
			g.participants = map[gameParticipant]bool{gameParticipantKey(event.actor, event.display): true}
		case state.Status == "won" || state.Status == "lost" || state.Status == "stopped":
			g.round++
			g.kind = ""
			g.participants = nil
		case event.action == "guess":
			if len(g.participants) < 256 {
				g.participants[gameParticipantKey(event.actor, event.display)] = true
			}
		}
	}
	g.mu.Unlock()
	defer func() { g.mu.Lock(); g.runningActor = ""; g.runningCancel = nil; g.mu.Unlock() }()
	if ctx.Err() != nil || g.isBlocked(event.actor) {
		return
	}
	text := ""
	if result.err != nil {
		text = "game: " + observationText(result.err.Error(), 190)
	} else {
		text = renderGameReply(event.action, result.value.(gameResult))
	}
	if g.send != nil {
		// Preserve a tool turn's privacy; direct public game inputs get a new scope.
		if replyScope(ctx) == nil {
			ctx, _ = scopedTurn(ctx, event.actor)
		}
		if err := g.send(ctx, text); err != nil {
			result.err = err
			if ctx.Err() == nil {
				log.Printf("game reply: %v", err)
			}
		}
	}
}
func (g *gameChat) isBlocked(actor string) bool { return g.blocked != nil && g.blocked(actor) }
func gameParticipantKey(actor, display string) gameParticipant {
	key := gameParticipant{actor: strings.ToLower(actor)}
	if isDiscordActor(actor) {
		key.display = normalizeDiscordNickname(display)
	}
	return key
}
func (g *gameChat) reset() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.runningCancel != nil {
		g.runningCancel()
	}
	g.session++
	g.round++
	g.kind = ""
	g.participants = nil
	g.manager.mu.Lock()
	g.manager.active = nil
	g.manager.mu.Unlock()
}
func (g *gameChat) forget(actor string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if strings.EqualFold(g.runningActor, actor) && g.runningCancel != nil {
		g.runningCancel()
	}
	for participant := range g.participants {
		if strings.EqualFold(participant.actor, actor) {
			// Game clues derive from public guesses. Discard that round rather than
			// republish a newly blocked player's guesses or derived clues in a board.
			if g.runningCancel != nil {
				g.runningCancel()
			}
			g.round++
			g.kind = ""
			g.participants = nil
			g.manager.mu.Lock()
			g.manager.active = nil
			g.manager.mu.Unlock()
			break
		}
	}
}

func (g *gameChat) route(ctx context.Context, actor, display, text, username string, aliases []string) bool {
	if g == nil || ctx.Err() != nil {
		return false
	}
	input, addressed := gameAddress(text, username, aliases)
	if actor == consoleActor {
		addressed = true
	}
	lower := strings.ToLower(strings.Join(strings.Fields(input), " "))
	action, raw := "", json.RawMessage(`{}`)
	if addressed {
		if game := requestedGame(lower); game != "" {
			action = "start_game"
			raw, _ = json.Marshal(map[string]string{"game": game})
		}
	}
	switch lower {
	case "stop game", "stop the game", "end game", "end the game", "stop hangman", "stop wordle", "stop anagram", "stop number":
		action = "stop_game"
	case "game status", "game board", "show game", "show the game":
		action = "game_status"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if action == "" && (lower == "status" || lower == "board") && g.kind != "" && (addressed || g.participants[gameParticipantKey(actor, display)]) {
		action = "game_status"
	}
	if action == "" {
		candidate := input
		explicit := false
		if fields := strings.Fields(input); len(fields) >= 1 && strings.EqualFold(fields[0], "guess") {
			candidate = strings.TrimSpace(strings.TrimPrefix(input, fields[0]))
			explicit = true
		}
		single := len(strings.Fields(candidate)) == 1
		// Anagrams are open to the whole chat: a shaped answer joins immediately.
		// Other games retain opt-in bare guesses to avoid consuming ambient words.
		bare := gameShapedGuess(g.kind, g.length, candidate) && (g.kind == "anagram" || g.participants[gameParticipantKey(actor, display)] && !endsConversation(candidate))
		directed := addressed && len(candidate) <= 15 && (asciiGameWord(candidate) || gameShapedGuess("number", 0, candidate))
		eligible := explicit || g.kind != "" && single && (directed || bare)
		if !eligible {
			return false
		}
		action = "guess"
		raw, _ = json.Marshal(map[string]string{"guess": candidate})
	}
	event := gameInput{ctx: ctx, actor: actor, display: display, action: action, raw: raw, session: g.session, round: g.round}
	select {
	case g.inputs <- event:
	default:
		log.Print("game input queue full; no attempt consumed")
	}
	return true
}
func gameShapedGuess(kind string, length int, text string) bool {
	if kind == "number" {
		if text == "" || len(text) > 3 {
			return false
		}
		for i := range len(text) {
			if text[i] < '0' || text[i] > '9' {
				return false
			}
		}
		return true
	}
	return asciiGameWord(text) && (len(text) == length || kind == "hangman" && len(text) == 1)
}
func requestedGame(text string) string {
	for _, prefix := range []string{"start ", "play ", "lets play ", "let's play "} {
		if strings.HasPrefix(text, prefix) {
			name := strings.TrimPrefix(strings.TrimPrefix(text, prefix), "a ")
			name = strings.TrimSuffix(name, " game")
			switch name {
			case "hangman", "wordle", "anagram", "number":
				return name
			case "anagrams":
				return "anagram"
			case "higher lower", "higher/lower", "higher or lower":
				return "number"
			}
		}
	}
	return ""
}

// Only a leading bot vocative is removed: "Bob, grace said ..." is not an
// invitation to consume another player's words as a game command.
func gameAddress(text, username string, aliases []string) (string, bool) {
	original := strings.TrimSpace(text)
	text = original
	for _, greeting := range []string{"hey ", "hi ", "hello "} {
		if strings.HasPrefix(strings.ToLower(text), greeting) {
			text = strings.TrimSpace(text[len(greeting):])
			break
		}
	}
	text = strings.TrimPrefix(text, "@")
	match := func(name string) (string, bool) {
		if name == "" || len(text) < len(name) || !strings.EqualFold(text[:len(name)], name) {
			return "", false
		}
		if len(text) == len(name) {
			return "", true
		}
		if !strings.ContainsRune(" ,:!", rune(text[len(name)])) {
			return "", false
		}
		return strings.TrimLeft(text[len(name):], " ,:!"), true
	}
	if rest, ok := match(username); ok {
		return rest, true
	}
	for _, name := range aliases {
		if rest, ok := match(name); ok {
			return rest, true
		}
	}
	return original, false
}

// A route consumes a message only after capture/moderation. Forgetting the LLM
// engagement prevents a pending model reply from narrating stale game state.
func (r *chatRouter) routeGame(ctx context.Context, actor, display, text, username string) bool {
	if r.games == nil || !r.games.route(ctx, actor, display, text, username, r.aliases) {
		return false
	}
	r.assistant.engagements.forget(actor)
	return true
}
