package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestMemorySourceOwnershipAndUpdatePrivacy(t *testing.T) {
	s := testPlayerStore(t)
	ctx := context.Background()
	h := s.history()
	privateID, err := h.append("private", "alice", "the vault is east", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	publicID, err := h.append("player", "bob", "the market is west", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	args := func(id int64, visibility string, sources ...uint64) json.RawMessage {
		data, _ := json.Marshal(map[string]any{"id": id, "subject": "places", "fact": "remember the vault", "visibility": visibility, "source_event_ids": sources})
		return data
	}
	if _, err = s.rememberTool(ctx, "bob", args(0, "private", privateID)); err == nil {
		t.Fatal("another player's source became readable")
	}
	if _, err = s.rememberTool(ctx, "alice", args(0, "public", privateID)); err == nil {
		t.Fatal("private source promoted to public")
	}
	value, err := s.rememberTool(ctx, "alice", args(0, "private", privateID))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(value)
	var fact memoryFact
	if err = json.Unmarshal(data, &fact); err != nil {
		t.Fatal(err)
	}
	if _, err = s.updateMemoryTool(ctx, "bob", args(fact.ID, "private", publicID)); err == nil {
		t.Fatal("other player overwrote private fact")
	}
	// Omitting/replacing sources cannot wash prior private provenance away.
	if _, err = s.updateMemoryTool(ctx, "alice", args(fact.ID, "public", publicID)); err == nil {
		t.Fatal("update laundered private provenance")
	}
	if _, err = s.updateMemoryTool(ctx, "alice", args(fact.ID, "private", publicID)); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Entries []memoryFact `json:"entries"`
	}
	if err = storeToolResult(t, s, "memory_search", "bob", `{}`, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 0 {
		t.Fatal("private fact leaked")
	}
	if err = storeToolResult(t, s, "memory_search", "alice", `{}`, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 || len(result.Entries[0].SourceIDs) != 2 || result.Entries[0].Attribution != "alice" {
		t.Fatal("update discarded source attribution")
	}
	raw, _ := json.Marshal(map[string]int64{"id": fact.ID})
	if _, err = s.forgetMemoryTool(ctx, "bob", raw); err == nil {
		t.Fatal("another player's private note deleted")
	}
	if _, err = s.forgetMemoryTool(ctx, "alice", raw); err != nil {
		t.Fatal(err)
	}
	if err = storeToolResult(t, s, "memory_search", "alice", `{}`, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 0 {
		t.Fatal("forgotten note retrievable")
	}
}
func TestMemoryPublicSearchUsesLiteralSQLParameters(t *testing.T) {
	s := testPlayerStore(t)
	ctx := context.Background()
	literal := "x' OR 1=1 -- %_"
	for _, text := range []string{literal, "another fact"} {
		data, _ := json.Marshal(map[string]any{"subject": "facts", "fact": text, "visibility": "public"})
		if _, err := s.rememberTool(ctx, "alice", data); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(map[string]string{"query": literal})
	value, err := s.searchMemoryTool(ctx, "bob", raw)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(value)
	var result struct {
		Entries []memoryFact `json:"entries"`
	}
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 || result.Entries[0].Fact != literal {
		t.Fatal("literal search changed SQL semantics")
	}
	raw, _ = json.Marshal(map[string]any{"id": result.Entries[0].ID, "subject": "facts", "fact": "changed", "visibility": "public"})
	if _, err = s.updateMemoryTool(ctx, "bob", raw); err == nil {
		t.Fatal("public visibility granted write authority")
	}
}
