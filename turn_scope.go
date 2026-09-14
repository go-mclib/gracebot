package main

import (
	"context"
	"sync/atomic"
)

// The model cannot choose or clear this bit. Once a turn reads private data,
// every reply in that turn uses the authenticated actor's private destination.
type turnScope struct {
	actor   string
	private atomic.Bool
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
