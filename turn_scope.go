package main

import (
	"context"
	"fmt"
	"sync/atomic"
)

// Private data defaults to a private reply for this turn, not future turns.
// An authenticated player may explicitly request a public reply through respond;
// the sensitivity bit remains set so private details never enter automatic context.
type turnScope struct {
	actor    string
	display  string // Untrusted display label for local game participation, never authority.
	private  atomic.Bool
	audience atomic.Uint32
}
type turnScopeKey struct{}
type publicHistoryKey struct{}

func scopedTurn(ctx context.Context, actor string) (context.Context, *turnScope) {
	scope := &turnScope{actor: actor}
	if actor == consoleActor {
		scope.private.Store(true)
	}
	return context.WithValue(ctx, turnScopeKey{}, scope), scope
}
func privateToolData(ctx context.Context) {
	if scope, ok := ctx.Value(turnScopeKey{}).(*turnScope); ok {
		scope.private.Store(true)
	}
}
func replyScope(ctx context.Context) *turnScope {
	scope, _ := ctx.Value(turnScopeKey{}).(*turnScope)
	return scope
}

func (s *turnScope) setAudience(audience string) error {
	switch audience {
	case "", "auto":
		s.audience.Store(0)
	case "public":
		if s.private.Load() && !validUsername(s.actor) {
			return fmt.Errorf("private data cannot be published for an unauthenticated chat actor")
		}
		s.audience.Store(1)
	case "private":
		if !validUsername(s.actor) && s.actor != consoleActor && s.actor != "@server" {
			return fmt.Errorf("this chat source has no authenticated private reply destination")
		}
		s.audience.Store(2)
	default:
		return fmt.Errorf("audience must be auto, public or private")
	}
	return nil
}

func (s *turnScope) privateReply() bool {
	if s.actor == consoleActor {
		return true
	}
	switch s.audience.Load() {
	case 1:
		return false
	case 2:
		return true
	default:
		return s.private.Load()
	}
}
