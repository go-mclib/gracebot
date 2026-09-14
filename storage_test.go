package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLegacyImportAtomicIdempotentAndUntimed(t *testing.T) {
	s := testPlayerStore(t)
	dir := t.TempDir()
	players, chat := filepath.Join(dir, "players.json"), filepath.Join(dir, "chat.jsonl")
	playerData := []byte(`{"Alice":{"wordle_points":4,"ranks":["Builder","ADMIN"],"messages":99,"word_counts":{"ghost":8},"death_counts":{"fall":7}}}`)
	if err := os.WriteFile(players, playerData, 0600); err != nil {
		t.Fatal(err)
	}
	entry, _ := json.Marshal(chatEntry{ID: 1, At: time.Now(), Kind: "private", Sender: "Alice", Text: "legacy secret"})
	if err := os.WriteFile(chat, append(append([]byte{}, entry...), []byte("\n{broken\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.importLegacy(players, chat); err == nil {
		t.Fatal("malformed import committed")
	}
	if s.hasRank("alice", "builder") {
		t.Fatal("partial import granted rank")
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM imports").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("failed import marked complete")
	}
	valid := append(append([]byte{}, entry...), '\n')
	if err := os.WriteFile(chat, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.importLegacy(players, chat); err != nil {
		t.Fatal(err)
	}
	if err := s.importLegacy(players, chat); err != nil {
		t.Fatal(err)
	}
	if !s.hasRank("alice", "builder") || s.hasRank("alice", "admin") || s.addPoint("alice", "wordle") != 5 {
		t.Fatal("legacy scores/ranks not preserved safely")
	}
	entries := historyEntries(t, s.history(), "alice", `{}`)
	if len(entries) != 1 || entries[0].Source != "legacy_chat" || entries[0].SourceID == nil || *entries[0].SourceID != 1 {
		t.Fatalf("duplicate/missing source attribution: %+v", entries)
	}
	if len(historyEntries(t, s.history(), "bob", `{}`)) != 0 {
		t.Fatal("legacy private message exposed")
	}
	var stats struct {
		Messages int64 `json:"messages"`
		Words    int64 `json:"words"`
		Deaths   int64 `json:"total_deaths"`
	}
	if err := storeToolResult(t, s, "player_stats", "alice", `{}`, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Messages != 0 || stats.Words != 0 || stats.Deaths != 0 {
		t.Fatalf("untimed aggregates or private chat contributed to public statistics: %+v", stats)
	}
	data, err := os.ReadFile(players)
	if err != nil || string(data) != string(playerData) {
		t.Fatal("legacy player file modified")
	}
	data, err = os.ReadFile(chat)
	if err != nil || string(data) != string(valid) {
		t.Fatal("legacy chat file modified")
	}
	if err = os.WriteFile(chat, append(valid, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.importLegacy(players, chat); err == nil {
		t.Fatal("changed source silently reimported")
	}
}
func TestLegacyInterruptedFinalLineRollsBack(t *testing.T) {
	s := testPlayerStore(t)
	path := filepath.Join(t.TempDir(), "chat.jsonl")
	data, _ := json.Marshal(chatEntry{ID: 1, At: time.Now(), Kind: "player", Sender: "alice", Text: "hello"})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.importLegacy("", path); err == nil {
		t.Fatal("interrupted final record imported")
	}
	if len(historyEntries(t, s.history(), "alice", `{}`)) != 0 {
		t.Fatal("interrupted import left rows")
	}
}
func TestRetentionCutoffArchivesAndDurableFacts(t *testing.T) {
	s := testPlayerStore(t)
	now := time.Now().UTC()
	cutoff := historyCutoff(now)
	if cutoff != now.Truncate(24*time.Hour).AddDate(0, 0, -6) {
		t.Fatal("wrong seven-date cutoff")
	}
	h := s.history()
	expired, err := h.append("player", "alice", "expired word", cutoff.Add(-time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	retained, err := h.append("private", "alice", "retained secret", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.append("player", "alice", "today", now); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"subject": "alice", "fact": "private persistent fact", "visibility": "private", "source_event_ids": []uint64{retained}})
	if _, err = s.rememberTool(context.Background(), "alice", args); err != nil {
		t.Fatal(err)
	}
	s.addPoint("alice", "wordle")
	if err = storeToolResult(t, s, "set_rank", "RealAdmin", `{"player":"alice","rank":"builder","action":"add"}`, nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "archives")
	if err = os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	expiredArchive := filepath.Join(dir, "gracebot-chat-"+cutoff.AddDate(0, 0, -1).Format("2006-01-02")+".jsonl.gz")
	if err = os.WriteFile(expiredArchive, []byte("expired"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.maintenance(context.Background(), now, dir); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.db.QueryRow("SELECT count(*) FROM events WHERE id=?", expired).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired row retained")
	}
	if err = s.db.QueryRow("SELECT count(*) FROM events WHERE id=?", retained).Scan(&count); err != nil || count != 1 {
		t.Fatal("inclusive boundary deleted")
	}
	if _, err = os.Stat(expiredArchive); !os.IsNotExist(err) {
		t.Fatal("expired archive remains")
	}
	archive := filepath.Join(dir, "gracebot-chat-"+cutoff.Format("2006-01-02")+".jsonl.gz")
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	zip, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(zip)
	zip.Close()
	f.Close()
	if err != nil || !strings.Contains(string(data), "retained secret") || strings.Contains(string(data), "expired word") {
		t.Fatal("archive cutoff lost privacy provenance or includes expired rows")
	}
	for path, want := range map[string]os.FileMode{dir: 0700, archive: 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("unsafe permissions on %s", path)
		}
	}
	if _, err = h.appendOwned("system", "", "@unattributed", "late private feedback", cutoff.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = s.maintenance(context.Background(), now, dir); err != nil {
		t.Fatal(err)
	}
	f, err = os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	zip, err = gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	data, err = io.ReadAll(zip)
	zip.Close()
	f.Close()
	if err != nil || !strings.Contains(string(data), "late private feedback") {
		t.Fatal("late arrival did not replace completed archive")
	}
	if err = s.maintenance(context.Background(), now.AddDate(0, 0, 7), dir); err != nil {
		t.Fatal(err)
	}
	if !s.hasRank("alice", "builder") || s.addPoint("alice", "wordle") != 2 {
		t.Fatal("retention deleted ranks/scores")
	}
	var facts struct {
		Entries []memoryFact `json:"entries"`
	}
	if err = storeToolResult(t, s, "memory_search", "alice", `{}`, &facts); err != nil {
		t.Fatal(err)
	}
	if len(facts.Entries) != 1 || len(facts.Entries[0].SourceIDs) != 1 {
		t.Fatal("retention deleted curated memory/provenance")
	}
	args, _ = json.Marshal(map[string]any{"id": facts.Entries[0].ID, "subject": "alice", "fact": "public leak", "visibility": "public"})
	if _, err = s.updateMemoryTool(context.Background(), "alice", args); err == nil {
		t.Fatal("expired private source lost its visibility restriction")
	}
}

func TestLegacyAmbiguousChatRemainsQuarantined(t *testing.T) {
	s := testPlayerStore(t)
	now := time.Now().UTC()
	entries := []chatEntry{
		{ID: 1, At: now, Kind: "system", Text: "Alice whispers to you: secret"},
		{ID: 2, At: now, Kind: "disguised", Sender: "Alice", Text: "Private command balance: 123"},
		{ID: 3, At: now, Kind: "system", Text: "Welcome to the server"},
		{ID: 4, At: now, Kind: "disguised", Sender: "Alice", Text: "Hello everyone"},
		{ID: 5, At: now, Kind: "player", Sender: "alice", Text: "Public player chat"},
		{ID: 6, At: now, Kind: "bot", Text: "Public bot chat"},
		{ID: 7, At: now, Kind: "system", Owner: "alice", Text: "Owned feedback"},
	}
	var data strings.Builder
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		data.Write(line)
		data.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), "chat.jsonl")
	if err := os.WriteFile(path, []byte(data.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.importLegacy("", path); err != nil {
		t.Fatal(err)
	}
	for _, original := range entries {
		saved, err := scanChatEntry(s.db.QueryRow("SELECT "+eventColumns+" FROM events WHERE source='legacy_chat' AND source_id=?", original.ID))
		if err != nil {
			t.Fatal(err)
		}
		owner := original.Owner
		if original.ID <= 4 {
			owner = "@unattributed"
		}
		if saved.Owner != owner || saved.Kind != original.Kind || saved.Text != original.Text || saved.SourceID == nil || *saved.SourceID != original.ID {
			t.Fatalf("legacy record/provenance not preserved: %+v", saved)
		}
		if original.ID > 4 {
			continue
		}
		args, err := json.Marshal(map[string]any{"subject": "alice", "fact": "leaked feedback", "visibility": "public", "source_event_ids": []uint64{saved.ID}})
		if err != nil {
			t.Fatal(err)
		}
		for _, actor := range []string{"alice", "bob", "RealAdmin", "@console"} {
			if _, err := s.rememberTool(context.Background(), actor, args); err == nil {
				t.Fatalf("%s promoted quarantined source %d", actor, saved.ID)
			}
		}
	}
	for _, actor := range []string{"bob", "RealAdmin", "@console"} {
		got := historyEntries(t, s.history(), actor, `{}`)
		if len(got) != 2 || got[0].SourceID == nil || *got[0].SourceID != 6 || got[1].SourceID == nil || *got[1].SourceID != 5 {
			t.Fatalf("%s retrieved ambiguous legacy chat: %+v", actor, got)
		}
	}
	if got := historyEntries(t, s.history(), "alice", `{}`); len(got) != 3 || got[0].Owner != "alice" {
		t.Fatalf("explicit legacy owner lost access: %+v", got)
	}
}

func TestPresenceFailureDoesNotCreditOfflineGaps(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "disconnect"
		if partial {
			name = "partial_roster"
		}
		t.Run(name, func(t *testing.T) {
			s := testPlayerStore(t)
			base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			roster := []string{"alice"}
			if partial {
				roster = append(roster, "bob")
			}
			if err := s.recordPresence(roster, base); err != nil {
				t.Fatal(err)
			}
			if err := s.recordPresence(roster, base.Add(10*time.Second)); err != nil {
				t.Fatal(err)
			}
			trigger := "CREATE TRIGGER fail_presence BEFORE UPDATE ON presence BEGIN SELECT RAISE(ABORT, 'forced presence failure'); END"
			if partial {
				// Updates run before inserts, so this also proves rollback of
				// partially executed interval updates.
				trigger = "CREATE TRIGGER fail_presence BEFORE INSERT ON presence WHEN NEW.player='charlie' BEGIN SELECT RAISE(ABORT, 'forced presence failure'); END"
			}
			if _, err := s.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			var err error
			if partial {
				err = s.recordPresence([]string{"bob", "charlie"}, base.Add(20*time.Second))
			} else {
				err = s.disconnectPresence(base.Add(20 * time.Second))
			}
			if err == nil {
				t.Fatal("failure injection did not abort presence transaction")
			}
			stats, err := s.retainedStats(context.Background(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			if p := stats["alice"]; p == nil || p.Online || p.OnlineSeconds != 10 || p.Sessions != 1 {
				t.Fatalf("failed removal kept alice online or changed durable end: %+v", p)
			}
			if partial && (stats["charlie"] == nil || !stats["charlie"].Online || stats["charlie"].OnlineSeconds != 0) {
				t.Fatalf("failed insert lost observed roster or credited undurable interval: %+v", stats["charlie"])
			}
			if _, err := s.db.Exec("DROP TRIGGER fail_presence"); err != nil {
				t.Fatal(err)
			}
			if err := s.maintenance(context.Background(), base.Add(30*time.Minute), t.TempDir()); err != nil {
				t.Fatal(err)
			}
			stats, err = s.retainedStats(context.Background(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			if p := stats["alice"]; p == nil || p.Online || p.OnlineSeconds != 10 || p.Sessions != 1 {
				t.Fatalf("maintenance resurrected disconnected interval: %+v", p)
			}
			if err := s.recordPresence(roster, base.Add(40*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := s.disconnectPresence(base.Add(40*time.Minute + 5*time.Second)); err != nil {
				t.Fatal(err)
			}
			stats, err = s.retainedStats(context.Background(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			if p := stats["alice"]; p == nil || p.Online || p.OnlineSeconds != 15 || p.Sessions != 2 {
				t.Fatalf("reconnect credited offline gap: %+v", p)
			}
			if partial {
				if p := stats["bob"]; p == nil || p.Online || p.OnlineSeconds != 2405 || p.Sessions != 1 {
					t.Fatalf("continuously observed player lost interval: %+v", p)
				}
				if p := stats["charlie"]; p == nil || p.Online || p.OnlineSeconds != 600 || p.Sessions != 1 {
					t.Fatalf("failed initial insert created phantom interval: %+v", p)
				}
			}
		})
	}
}
