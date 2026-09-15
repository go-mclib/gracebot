package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestActorBlockPersistenceExpiryAndPrecedence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path, admins := filepath.Join(dir, "state.sqlite"), filepath.Join(dir, "admins.json")
	s, err := openPlayerStore(path, admins)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	until := at.Add(10 * time.Minute)
	block, err := s.setActorBlock(ctx, " Alice ", consoleActor, "manual moderation", nil, at)
	if err != nil || block.Actor != "alice" || !block.CreatedAt.Equal(at) {
		t.Fatalf("block: %+v, %v", block, err)
	}
	block, err = s.setActorBlock(ctx, "ALICE", "@automatic", "rapid messages", &until, at.Add(time.Second))
	if err != nil || block.By != consoleActor || block.ExpiresAt != nil || block.Reason != "manual moderation" {
		t.Fatalf("automatic replaced permanent manual: %+v, %v", block, err)
	}
	if err = s.close(); err != nil {
		t.Fatal(err)
	}
	s, err = openPlayerStore(path, admins)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if !s.actorBlocked("ALICE", until.Add(time.Hour)) {
		t.Fatal("permanent block did not survive restart")
	}
	if changed, err := s.clearActorBlock(ctx, " ALICE ", consoleActor); err != nil || !changed {
		t.Fatalf("unblock: %v, %v", changed, err)
	}
	if s.actorBlocked("alice", at) {
		t.Fatal("cleared block active")
	}
	if changed, err := s.clearActorBlock(ctx, "alice", consoleActor); err != nil || changed {
		t.Fatalf("repeated unblock: %v, %v", changed, err)
	}
	if _, err = s.setActorBlock(ctx, "alice", consoleActor, "temporary manual", &until, at); err != nil {
		t.Fatal(err)
	}
	later := until.Add(time.Hour)
	block, err = s.setActorBlock(ctx, "alice", "@automatic", "rapid messages", &later, at.Add(time.Minute))
	if err != nil || block.By != consoleActor || !block.ExpiresAt.Equal(until) {
		t.Fatalf("automatic replaced temporary manual: %+v, %v", block, err)
	}
	if !s.actorBlocked("alice", until.Add(-time.Nanosecond)) || s.actorBlocked("alice", until) {
		t.Fatal("expiry boundary is not exclusive")
	}
	blocks, err := s.blockedActors(ctx, until)
	if err != nil || len(blocks) != 0 {
		t.Fatalf("expired block listed: %+v, %v", blocks, err)
	}
	block, err = s.setActorBlock(ctx, "alice", "@automatic", "rapid messages", &later, until)
	if err != nil || block.By != "@automatic" || !block.ExpiresAt.Equal(later) {
		t.Fatalf("expired manual prevented new block: %+v, %v", block, err)
	}
	shorter := until.Add(time.Minute)
	block, err = s.setActorBlock(ctx, "alice", "@automatic", "rapid messages", &shorter, until)
	if err != nil || !block.ExpiresAt.Equal(later) {
		t.Fatalf("automatic shortened block: %+v, %v", block, err)
	}
	if _, err = s.setActorBlock(ctx, "@discord:alice", consoleActor, "invalid identity", nil, at); err == nil {
		t.Fatal("nickname-specific block accepted")
	}
	if _, err = s.db.Exec("DROP TABLE actor_blocks"); err != nil {
		t.Fatal(err)
	}
	if !s.actorBlocked("alice", at) {
		t.Fatal("database error failed open")
	}
}

func TestBlockedStorageDiscardsWritesAndHidesRetainedHistory(t *testing.T) {
	s := testPlayerStore(t)
	h := s.history()
	ctx, now := context.Background(), time.Now().UTC()
	if _, err := h.append("player", "alice", "old excluded token", now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.append("private", "alice", "old private", now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.appendOwned("bot", "GraceBot", "alice", "old attributed answer", now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.append("player", "bob", "visible", now); err != nil {
		t.Fatal(err)
	}
	if err := s.recordPresence([]string{"alice", "bob"}, now); err != nil {
		t.Fatal(err)
	}
	s.addPoint("alice", "wordle")
	if _, err := s.setActorBlock(ctx, "ALICE", consoleActor, "manual", nil, now); err != nil {
		t.Fatal(err)
	}
	for _, event := range []chatEntry{
		{Kind: "player", Sender: "Alice", Text: "new unsaved token"},
		{Kind: "private", Sender: "alice", Owner: "alice", Text: "new unsaved private"},
		{Kind: "bot", Sender: "GraceBot", Owner: "Alice", Text: "new unsaved answer"},
		{Kind: "death", Sender: "ALICE", Text: "new unsaved death"},
	} {
		if id, err := h.appendOwned(event.Kind, event.Sender, event.Owner, event.Text, now.Add(-time.Hour)); err != nil || id != 0 {
			t.Fatalf("blocked write saved: %d, %v", id, err)
		}
	}
	if err := s.recordPresence([]string{"alice", "bob"}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{`{}`, `{"query":"old"}`, `{"regex":"old"}`, `{"player":"ALICE"}`} {
		for _, event := range historyEntries(t, h, "alice", args) {
			if event.Sender == "alice" || event.Owner == "alice" {
				t.Fatalf("blocked retained history visible: %+v", event)
			}
		}
	}
	if strings.Contains(h.recentContext(0, "bob"), "old excluded") {
		t.Fatal("blocked context leaked")
	}
	stats, err := s.retainedStats(ctx, "bob")
	if err != nil || stats["alice"] != nil || stats["bob"] == nil || stats["bob"].Messages != 1 {
		t.Fatalf("blocked statistics leaked: %+v, %v", stats, err)
	}
	var word struct {
		Count int64 `json:"count"`
	}
	if err = storeToolResult(t, s, "word_count", "bob", `{"word":"excluded"}`, &word); err != nil || word.Count != 0 {
		t.Fatalf("blocked token counted: %+v, %v", word, err)
	}
	var top struct {
		Entries []struct {
			Word string `json:"word"`
		} `json:"entries"`
	}
	if err = storeToolResult(t, s, "top_words", "bob", `{}`, &top); err != nil {
		t.Fatal(err)
	}
	if len(top.Entries) != 1 || top.Entries[0].Word != "visible" {
		t.Fatalf("blocked top words: %+v", top)
	}
	if _, err = s.clearActorBlock(ctx, "alice", consoleActor); err != nil {
		t.Fatal(err)
	}
	entries := historyEntries(t, h, "alice", `{}`)
	if len(entries) != 4 {
		t.Fatalf("pre-block records erased or blocked writes persisted: %+v", entries)
	}
	stats, err = s.retainedStats(ctx, "bob")
	if err != nil || stats["alice"].Online || stats["alice"].OnlineSeconds != 0 || stats["alice"].Sessions != 1 {
		t.Fatalf("blocked presence advanced: %+v, %v", stats["alice"], err)
	}
	if got := historyEntries(t, h, "bob", `{"query":"new unsaved"}`); len(got) != 0 {
		t.Fatalf("blocked content survived unblock: %+v", got)
	}
}

func TestBlockedMemorySubjectsOwnersAndExpiredSources(t *testing.T) {
	s := testPlayerStore(t)
	ctx, now := context.Background(), time.Now()
	source, err := s.history().append("player", "alice", "source fact", now)
	if err != nil {
		t.Fatal(err)
	}
	remember := func(actor, subject, fact string, sources []uint64) (any, error) {
		raw, _ := json.Marshal(memoryWriteArgs{Subject: subject, Fact: fact, Visibility: "public", SourceIDs: sources})
		return s.rememberTool(ctx, actor, raw)
	}
	for _, note := range []struct {
		actor, subject, fact string
		sources              []uint64
	}{
		{"bob", " ALICE ", "subject fact", nil},
		{"alice", "build", "owner fact", nil},
		{"bob", "build", "source-derived fact", []uint64{source}},
		{"bob", "build", "safe fact", nil},
	} {
		if _, err = remember(note.actor, note.subject, note.fact, note.sources); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.setActorBlock(ctx, "alice", consoleActor, "manual", nil, now); err != nil {
		t.Fatal(err)
	}
	for _, note := range []struct {
		actor, subject string
		sources        []uint64
	}{{"alice", "build", nil}, {"bob", "ALICE", nil}, {"bob", "build", []uint64{source}}} {
		if _, err = remember(note.actor, note.subject, "blocked new fact", note.sources); err == nil {
			t.Fatal("blocked attribution saved")
		}
	}
	if _, err = s.updateMemoryTool(ctx, "bob", json.RawMessage(`{"id":3,"subject":"safe","fact":"laundered provenance","visibility":"public"}`)); err == nil {
		t.Fatal("blocked source memory was rewritten")
	}
	if _, err = s.db.Exec("DELETE FROM events WHERE id=?", source); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Entries []memoryFact `json:"entries"`
	}
	if err = storeToolResult(t, s, "memory_search", "bob", `{}`, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 || result.Entries[0].Fact != "safe fact" {
		t.Fatalf("blocked memory leaked after source expiry: %+v", result)
	}
	if _, err = s.clearActorBlock(ctx, "alice", consoleActor); err != nil {
		t.Fatal(err)
	}
	if err = storeToolResult(t, s, "memory_search", "bob", `{}`, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 4 {
		t.Fatalf("pre-block memories erased or rejected writes saved: %+v", result)
	}
}

func TestDiscordExistingDatabaseMigratesWithoutMinecraftAlias(t *testing.T) {
	ctx, now := context.Background(), time.Now()
	dir := t.TempDir()
	path, admins := filepath.Join(dir, "state.sqlite"), filepath.Join(dir, "admins.json")
	s, err := openPlayerStore(path, admins)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("ALTER TABLE events DROP COLUMN display_name"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO events(id,at,kind,sender,text) VALUES(1,?,'discord','@discord:RealAdmin','original message'),(2,?,'player','realadmin','Minecraft message')", now.UnixNano(), now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO memories(id,subject,fact,owner,visibility,attribution,created_at,updated_at) VALUES(1,'bridge','Discord source fact','bob','public','bob',?,?)", now.UnixNano(), now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("INSERT INTO memory_sources(memory_id,event_id,owner,sender,at) VALUES(1,1,'','@discord:RealAdmin',?)", now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err = s.close(); err != nil {
		t.Fatal(err)
	}
	s, err = openPlayerStore(path, admins)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	entries := historyEntries(t, s.history(), "bob", `{"kind":"discord"}`)
	if len(entries) != 1 || entries[0].Sender != discordBridgeActor || entries[0].DisplayName != "RealAdmin" || entries[0].Text != "original message" {
		t.Fatalf("bad migrated Discord entry: %+v", entries)
	}
	if _, err = s.setActorBlock(ctx, discordBridgeActor, consoleActor, "bridge moderation", nil, now); err != nil {
		t.Fatal(err)
	}
	if id, err := s.history().appendDiscord("DifferentNickname", "blocked Discord message", now); err != nil || id != 0 {
		t.Fatalf("bridge block bypassed: %d, %v", id, err)
	}
	entries = historyEntries(t, s.history(), "bob", `{}`)
	if len(entries) != 1 || entries[0].Sender != "realadmin" {
		t.Fatalf("bridge block aliased Minecraft or leaked history: %+v", entries)
	}
	var result struct {
		Entries []memoryFact `json:"entries"`
	}
	if err = storeToolResult(t, s, "memory_search", "bob", `{}`, &result); err != nil || len(result.Entries) != 0 {
		t.Fatalf("old Discord provenance leaked: %+v, %v", result, err)
	}
	if _, err = s.clearActorBlock(ctx, discordBridgeActor, consoleActor); err != nil {
		t.Fatal(err)
	}
	entries = historyEntries(t, s.history(), "bob", `{"kind":"discord"}`)
	if len(entries) != 1 {
		t.Fatalf("blocked Discord message saved: %+v", entries)
	}
}

func TestPersistentAdminGrantsRequireAuthenticatedAdministrators(t *testing.T) {
	dir := t.TempDir()
	path, admins := filepath.Join(dir, "state.sqlite"), filepath.Join(dir, "admins.json")
	if err := os.WriteFile(admins, []byte(`["RealAdmin"]`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := openPlayerStore(path, admins)
	if err != nil {
		t.Fatal(err)
	}
	grant := `{"player":"alice","rank":"ADMIN","action":"add"}`
	if err = storeToolResult(t, s, "set_rank", "RealAdmin", `{"player":"trusted","rank":"trusted","action":"add"}`, nil); err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"alice", "trusted", discordBridgeActor, "@discord:RealAdmin"} {
		if err = storeToolResult(t, s, "set_rank", actor, grant, nil); err == nil {
			t.Fatalf("%s granted admin", actor)
		}
	}
	if err = storeToolResult(t, s, "set_rank", "RealAdmin", grant, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.close(); err != nil {
		t.Fatal(err)
	}
	s, err = openPlayerStore(path, admins)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if !s.hasRank("ALICE", "admin") {
		t.Fatal("authorized grant did not survive restart")
	}
	if err = storeToolResult(t, s, "set_rank", "alice", `{"player":"bob","rank":"builder","action":"add"}`, nil); err != nil {
		t.Fatal(err)
	}
	if err = storeToolResult(t, s, "set_rank", "alice", `{"player":"RealAdmin","rank":"admin","action":"remove"}`, nil); err == nil || !s.hasRank("realadmin", "admin") {
		t.Fatal("bootstrap administrator revoked")
	}
	if err = storeToolResult(t, s, "set_rank", "RealAdmin", `{"player":"alice","rank":"admin","action":"remove"}`, nil); err != nil {
		t.Fatal(err)
	}
	if s.hasRank("alice", "admin") {
		t.Fatal("revoked persistent admin retained authority")
	}
	if err = storeToolResult(t, s, "set_rank", "alice", `{"player":"bob","rank":"admin","action":"add"}`, nil); err == nil {
		t.Fatal("revoked administrator granted rank")
	}
}

func TestMemoryRejectsLegacyDiscordIdentityAPI(t *testing.T) {
	s := testPlayerStore(t)
	ctx := context.Background()
	if _, err := s.rememberTool(ctx, "@discord:alice", json.RawMessage(`{"subject":"build","fact":"legacy caller","visibility":"public"}`)); err == nil {
		t.Fatal("legacy Discord actor saved memory")
	}
	if _, err := s.rememberTool(ctx, "bob", json.RawMessage(`{"subject":"@discord:alice","fact":"legacy subject","visibility":"public"}`)); err == nil {
		t.Fatal("legacy Discord subject saved memory")
	}
	if _, err := s.searchMemoryTool(ctx, "bob", json.RawMessage(`{"subject":"@discord:alice"}`)); err == nil {
		t.Fatal("legacy Discord subject search accepted")
	}
	if _, err := s.searchMemoryTool(ctx, "@discord:alice", json.RawMessage(`{}`)); err == nil {
		t.Fatal("legacy Discord actor searched memory")
	}
}
