package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestDisconnectBotRequiresLiveAuthenticatedAdminRequest(t *testing.T) {
	store := commandStore(t)
	for _, grant := range []string{
		`{"player":"Bob","rank":"trusted","action":"add"}`,
		`{"player":"Root","rank":"admin","action":"add"}`,
	} {
		if _, err := store.setRankTool(context.Background(), consoleActor, json.RawMessage(grant)); err != nil {
			t.Fatal(err)
		}
	}
	runCtx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	disconnect := operatorTools(store, shutdown)[0]
	for _, actor := range []string{"Alice", "Bob", discordBridgeActor, "@discord:Root", "@server", ""} {
		if _, err := disconnect.run(context.Background(), actor, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("unauthorized actor %q could stop the bot", actor)
		}
		if runCtx.Err() != nil {
			t.Fatalf("unauthorized actor %q stopped the bot", actor)
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := disconnect.run(cancelled, "Root", json.RawMessage(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled shutdown request returned %v", err)
	}
	if runCtx.Err() != nil {
		t.Fatal("cancelled admin request stopped the bot")
	}
	if _, err := disconnect.run(context.Background(), "Root", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(runCtx.Err(), context.Canceled) {
		t.Fatal("authenticated admin did not stop the bot runtime")
	}
}
