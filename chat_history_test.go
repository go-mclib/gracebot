package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func historyEntries(t *testing.T, h *chatHistory, actor, args string) []chatEntry {
	t.Helper()
	result, err := h.search(context.Background(), actor, json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Entries []chatEntry `json:"entries"`
	}
	if err = json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value.Entries
}
func TestHistoryOwnershipAndStructuredContext(t *testing.T) {
	s := testPlayerStore(t)
	h := s.history()
	now := time.Now()
	if _, err := h.append("player", "alice", "public fact", now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.append("private", "alice", "private secret", now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.appendOwned("command_feedback", "server", "alice", "command secret", now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.append("private_disguised", "Alice", "unattributed secret", now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.appendOwned("system", "", "@unattributed", "unclassified secret", now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.appendOwned("private", "alice", "", "missing provenance", now); err == nil {
		t.Fatal("unowned attributed private event accepted")
	}
	if got := historyEntries(t, h, "bob", `{}`); len(got) != 1 || got[0].Text != "public fact" {
		t.Fatalf("private data leaked: %+v", got)
	}
	if got := historyEntries(t, h, "alice", `{}`); len(got) != 3 {
		t.Fatalf("owned feedback unavailable: %+v", got)
	}
	if _, err := h.search(context.Background(), "@unattributed", json.RawMessage(`{}`)); err == nil {
		t.Fatal("internal archive namespace became authenticated actor")
	}
	contextData := h.recentContext(0, "bob")
	if !json.Valid([]byte(contextData)) || strings.Contains(contextData, "secret") {
		t.Fatalf("unsafe prompt context: %s", contextData)
	}
	if _, err := h.appendOwned("command_feedback", "server", "@server", "console output", now); err != nil {
		t.Fatal(err)
	}
	if s.hasRank("@server", "admin") {
		t.Fatal("server namespace gained admin")
	}
}

func TestHistoryPlayerFilterIsCaseInsensitiveWithoutLosingPrivacy(t *testing.T) {
	h := testPlayerStore(t).history()
	now := time.Now()
	if _, err := h.append("bot", "GraceLocal", "public reply", now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.appendOwned("bot", "GraceLocal", "alice", "private reply", now); err != nil {
		t.Fatal(err)
	}
	args := `{"player":"GRACELOCAL","kind":"bot"}`
	if got := historyEntries(t, h, "bob", args); len(got) != 1 || got[0].Text != "public reply" {
		t.Fatalf("case-insensitive sender filter lost public chat or exposed private chat: %+v", got)
	}
	if got := historyEntries(t, h, "alice", args); len(got) != 2 {
		t.Fatalf("owner could not retrieve mixed-case bot replies: %+v", got)
	}
}
func TestHistoryDuplicateTimestampCursorAndSQLParameters(t *testing.T) {
	s := testPlayerStore(t)
	h := s.history()
	now := time.Now()
	literal := "x' OR 1=1 -- %_"
	for i := range 5 {
		text := fmt.Sprintf("message %d", i)
		if i == 2 {
			text = literal
		}
		if _, err := h.append("player", "alice", text, now); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[uint64]bool{}
	var before uint64
	for {
		args, _ := json.Marshal(map[string]any{"before": before, "limit": 2, "since": now, "until": now.Add(time.Nanosecond)})
		result, err := h.search(context.Background(), "alice", args)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(result)
		var page struct {
			Entries []chatEntry `json:"entries"`
			Before  uint64      `json:"before"`
			More    bool        `json:"more"`
		}
		if err = json.Unmarshal(data, &page); err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Entries {
			if seen[e.ID] {
				t.Fatal("duplicate cursor entry")
			}
			seen[e.ID] = true
		}
		if !page.More {
			break
		}
		if page.Before == 0 || page.Before == before {
			t.Fatal("cursor did not advance")
		}
		before = page.Before
	}
	if len(seen) != 5 {
		t.Fatalf("timestamp pagination dropped records: %d", len(seen))
	}
	args, _ := json.Marshal(map[string]any{"query": literal})
	got := historyEntries(t, h, "alice", string(args))
	if len(got) != 1 || got[0].Text != literal {
		t.Fatalf("query interpolated SQL/wildcards: %+v", got)
	}
}
func TestHistoryRegexBoundAndContinuation(t *testing.T) {
	s := testPlayerStore(t)
	h := s.history()
	now := time.Now()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = insertChatEntry(context.Background(), tx, chatEntry{At: now, Kind: "system", Text: "needle", Source: "live"}); err != nil {
		t.Fatal(err)
	}
	for range historyScanLimit {
		if _, err = insertChatEntry(context.Background(), tx, chatEntry{At: now, Kind: "system", Text: "hay", Source: "live"}); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	value, err := h.search(context.Background(), "alice", json.RawMessage(`{"regex":"^needle$"}`))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(value)
	var page struct {
		Scanned int         `json:"scanned"`
		Limited bool        `json:"scan_limited"`
		Before  uint64      `json:"before"`
		Entries []chatEntry `json:"entries"`
	}
	if err = json.Unmarshal(data, &page); err != nil {
		t.Fatal(err)
	}
	if page.Scanned != historyScanLimit || !page.Limited || len(page.Entries) != 0 || page.Before == 0 {
		t.Fatalf("regex scan not bounded: %+v", page)
	}
	args, _ := json.Marshal(map[string]any{"regex": "^needle$", "before": page.Before})
	if got := historyEntries(t, h, "alice", string(args)); len(got) != 1 || got[0].Text != "needle" {
		t.Fatal("bounded scan lost older result")
	}
	for _, pattern := range []string{"[", strings.Repeat("a", 257)} {
		args, _ := json.Marshal(map[string]string{"regex": pattern})
		if _, err = h.search(context.Background(), "alice", args); err == nil {
			t.Fatal("invalid/unbounded regex accepted")
		}
	}
}

func TestHistoryRejectsUnrepresentableTimeBounds(t *testing.T) {
	s := testPlayerStore(t)
	h := s.history()
	now := time.Now().UTC()
	if _, err := h.append("player", "alice", "expired", historyCutoff(now).Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	retained, err := h.append("player", "alice", "retained", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, bound := range []string{"since", "until"} {
		for _, year := range []int{1969, 2201, 2263, 9999} {
			t.Run(fmt.Sprintf("%s_%d", bound, year), func(t *testing.T) {
				args, err := json.Marshal(map[string]time.Time{bound: time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)})
				if err != nil {
					t.Fatal(err)
				}
				if result, err := h.search(context.Background(), "alice", args); err == nil || result != nil {
					t.Fatalf("unsupported timestamp reached history query: result=%+v err=%v", result, err)
				}
			})
		}
	}
	got := historyEntries(t, h, "alice", `{"since":"1970-01-01T00:00:00Z","until":"2200-12-31T23:59:59Z"}`)
	if len(got) != 1 || got[0].ID != retained {
		t.Fatalf("supported date bounds bypassed retention: %+v", got)
	}
	if got := historyEntries(t, h, "alice", `{"since":"2200-01-01T00:00:00Z"}`); len(got) != 0 {
		t.Fatalf("representable future lower bound returned old events: %+v", got)
	}
}

func TestDiscordHistoryRemainsPublicAndCannotOwnPrivateData(t *testing.T) {
	store := testPlayerStore(t)
	h := store.history()
	now := time.Now()
	actor := "@discord:realadmin"
	if _, err := h.append("discord", actor, "public Discord fact", now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.append("player", "RealAdmin", "public Minecraft fact", now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.append("private", "RealAdmin", "private account secret", now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.appendOwned("system", "", "@unattributed", "quarantined secret", now); err != nil {
		t.Fatal(err)
	}
	// Even legacy/imported rows with an invalid Discord owner must not become
	// visible through an explicit search (which lacks the public-context flag).
	if _, err := store.db.Exec("INSERT INTO events(at,kind,sender,owner,text) VALUES(?,?,?,?,?)", now.UnixNano(), "bot", "GraceLocal", actor, "invalid owner secret"); err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{`{}`, `{"query":"secret"}`, `{"regex":"secret"}`, `{"kind":"private"}`, `{"player":"RealAdmin"}`} {
		for _, entry := range historyEntries(t, h, actor, args) {
			if entry.Owner != "" || strings.Contains(entry.Text, "secret") {
				t.Fatalf("Discord search exposed private data: %+v", entry)
			}
		}
	}
	if entries := historyEntries(t, h, actor, `{}`); len(entries) != 2 {
		t.Fatalf("Discord actor lost public conversation history: %+v", entries)
	}
	if entries := historyEntries(t, h, "alice", `{"kind":"discord","player":"@discord:realadmin"}`); len(entries) != 1 || entries[0].Text != "public Discord fact" {
		t.Fatalf("public Discord sender search failed: %+v", entries)
	}
	contextData := h.recentContext(0, actor)
	if !json.Valid([]byte(contextData)) || strings.Contains(contextData, "secret") || !strings.Contains(contextData, "public Discord fact") || !strings.Contains(contextData, "public Minecraft fact") {
		t.Fatalf("unsafe or missing Discord context: %s", contextData)
	}
	if _, err := storageActor(actor); err == nil {
		t.Fatal("Discord identity became a storage account")
	}
	if store.hasRank(actor, "admin") {
		t.Fatal("Discord nickname inherited Minecraft admin rank")
	}
	if _, err := h.appendOwned("bot", "GraceLocal", actor, "private Discord write", now); err == nil {
		t.Fatal("Discord identity became a private data owner")
	}
	if _, err := h.append("discord", "RealAdmin", "unnamespaced bridge input", now); err == nil {
		t.Fatal("Discord archive accepted a Minecraft identity")
	}
}
