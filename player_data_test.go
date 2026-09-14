package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func storeToolResult(t *testing.T, store *playerStore, name, actor, arguments string, target any) error {
	t.Helper()
	for _, tool := range store.tools() {
		if tool.name != name {
			continue
		}
		result, err := tool.run(context.Background(), actor, json.RawMessage(arguments))
		if err != nil {
			return err
		}
		if target != nil {
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(data, target); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	t.Fatalf("tool %s not found", name)
	return nil
}
func testPlayerStore(t *testing.T) *playerStore {
	t.Helper()
	dir := t.TempDir()
	admins := filepath.Join(dir, "admins.json")
	if err := os.WriteFile(admins, []byte(`["RealAdmin"]`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := openPlayerStore(filepath.Join(dir, "state.sqlite"), admins)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func TestSQLiteRanksScoresPersistAndRemainAuthorized(t *testing.T) {
	dir := t.TempDir()
	db, admins := filepath.Join(dir, "state.sqlite"), filepath.Join(dir, "admins.json")
	if err := os.WriteFile(admins, []byte(`["RealAdmin"]`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := openPlayerStore(db, admins)
	if err != nil {
		t.Fatal(err)
	}
	if !s.hasRank("REALADMIN", "ADMIN") || !s.hasRank("anyone", "default") {
		t.Fatal("reserved authority lost")
	}
	for _, args := range []string{`{"player":"alice","rank":"builder","action":"add"}`, `{"player":"alice","rank":"builder","action":"add","actor":"RealAdmin"}`} {
		if err := storeToolResult(t, s, "set_rank", "alice", args, nil); err == nil {
			t.Fatal("ordinary player changed rank")
		}
	}
	for _, rank := range []string{"ADMIN", "default", "x'); DROP TABLE ranks;--"} {
		raw, _ := json.Marshal(map[string]string{"player": "alice", "rank": rank, "action": "add"})
		if err := storeToolResult(t, s, "set_rank", "RealAdmin", string(raw), nil); err == nil {
			t.Fatal("invalid rank granted")
		}
	}
	if err := storeToolResult(t, s, "set_rank", "RealAdmin", `{"player":"alice","rank":"Builder","action":"add"}`, nil); err != nil {
		t.Fatal(err)
	}
	if s.addPoint("ALICE", "wordle") != 1 || s.addPoint("alice", "wordle") != 2 {
		t.Fatal("score increments lost")
	}
	if err = s.close(); err != nil {
		t.Fatal(err)
	}
	s, err = openPlayerStore(db, admins)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if !s.hasRank("alice", "builder") || s.hasRank("alice", "admin") || s.addPoint("alice", "wordle") != 3 {
		t.Fatal("score/rank persistence changed")
	}
	if err := storeToolResult(t, s, "set_rank", "RealAdmin", `{"player":"alice","rank":"builder","action":"remove"}`, nil); err != nil {
		t.Fatal(err)
	}
	if s.hasRank("alice", "builder") {
		t.Fatal("removed rank remains effective")
	}
}
func TestRetainedStatisticsDeriveFromVisibleEvents(t *testing.T) {
	s := testPlayerStore(t)
	h := s.history()
	now := time.Now()
	for _, e := range []chatEntry{{Kind: "player", Sender: "alice", Text: "Hello! hello world", At: now}, {Kind: "private", Sender: "bob", Owner: "bob", Text: "secret hello", At: now}, {Kind: "player", Sender: "alice", Text: "expired hello", At: historyCutoff(now).Add(-time.Nanosecond)}, {Kind: "bot", Sender: "GraceBot", Text: "hello", At: now}, {Kind: "death", Sender: "alice", Text: "death.attack.fall", At: now}} {
		if _, err := h.appendOwned(e.Kind, e.Sender, e.Owner, e.Text, e.At); err != nil {
			t.Fatal(err)
		}
	}
	var stats struct {
		Messages int64 `json:"messages"`
		Words    int64 `json:"words"`
		Deaths   int64 `json:"deaths"`
	}
	if err := storeToolResult(t, s, "server_stats", "alice", `{}`, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Messages != 1 || stats.Words != 3 || stats.Deaths != 1 {
		t.Fatalf("public stats: %+v", stats)
	}
	var count struct {
		Count int64 `json:"count"`
	}
	if err := storeToolResult(t, s, "word_count", "alice", `{"word":"HELLO!"}`, &count); err != nil {
		t.Fatal(err)
	}
	if count.Count != 2 {
		t.Fatalf("private/expired word leaked: %d", count.Count)
	}
	if err := storeToolResult(t, s, "word_count", "bob", `{"word":"hello"}`, &count); err != nil {
		t.Fatal(err)
	}
	if count.Count != 2 {
		t.Fatal("public statistics included the owner's private event")
	}
	if _, err := s.db.Exec("DELETE FROM events WHERE kind='player'"); err != nil {
		t.Fatal(err)
	}
	if err := storeToolResult(t, s, "word_count", "alice", `{"word":"hello"}`, &count); err != nil {
		t.Fatal(err)
	}
	if count.Count != 0 {
		t.Fatal("parallel counter survived event deletion")
	}
}
func TestPresenceExcludesDisconnectAndRestartGaps(t *testing.T) {
	dir := t.TempDir()
	db, admins := filepath.Join(dir, "state.sqlite"), filepath.Join(dir, "admins.json")
	s, err := openPlayerStore(db, admins)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err = s.recordPresence([]string{"Alice", "ALICE"}, base); err != nil {
		t.Fatal(err)
	}
	if err = s.recordPresence([]string{"alice"}, base.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: close the database without the orderly disconnect hook.
	if err = s.db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openPlayerStore(db, admins)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if err = s.recordPresence([]string{"alice"}, base.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = s.disconnectPresence(base.Add(30*time.Minute + 5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = s.disconnectPresence(base.Add(31 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	stats, err := s.retainedStats(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	p := stats["alice"]
	if p == nil || p.OnlineSeconds != 15 || p.Sessions != 2 || p.Online {
		t.Fatalf("offline gap counted: %+v", p)
	}
}
