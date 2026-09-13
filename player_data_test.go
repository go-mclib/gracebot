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
			if err := json.Unmarshal(data, target); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	t.Fatalf("tool %s not found", name)
	return nil
}

func TestPlayerStoreAdminAuthority(t *testing.T) {
	dir := t.TempDir()
	dataPath, adminPath := filepath.Join(dir, "players.json"), filepath.Join(dir, "admins.json")
	if err := os.WriteFile(dataPath, []byte(`{"attacker":{"ranks":["AdMiN","builder"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(adminPath, []byte(`["RealAdmin"]`), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := openPlayerStore(dataPath, adminPath)
	if err != nil {
		t.Fatal(err)
	}
	if store.hasRank("ATTACKER", "ADMIN") {
		t.Fatal("persisted admin rank granted authority")
	}
	if !store.hasRank("REALADMIN", "AdMiN") || !store.hasRank("anyone", "DEFAULT") {
		t.Fatal("case normalization changed reserved rank behavior")
	}
	if !store.hasRank("ATTACKER", "BUILDER") {
		t.Fatal("custom rank lost during normalization")
	}
	if err := storeToolResult(t, store, "set_rank", "attacker", `{"player":"attacker","rank":"singer","action":"add"}`, nil); err == nil {
		t.Fatal("non-admin changed a rank")
	}
	if err := storeToolResult(t, store, "set_rank", "attacker", `{"player":"attacker","rank":"singer","action":"add","actor":"RealAdmin"}`, nil); err == nil {
		t.Fatal("model arguments spoofed runtime actor")
	}
	for _, rank := range []string{"AdMiN", "DEFAULT"} {
		for _, action := range []string{"add", "remove"} {
			args, err := json.Marshal(map[string]string{"player": "attacker", "rank": rank, "action": action})
			if err != nil {
				t.Fatal(err)
			}
			if err := storeToolResult(t, store, "set_rank", "REALADMIN", string(args), nil); err == nil {
				t.Fatalf("reserved rank %s allowed %s", rank, action)
			}
		}
	}
	if err := storeToolResult(t, store, "set_rank", "REALADMIN", `{"player":"ATTACKER","rank":"Singer","action":"add"}`, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.save(); err != nil {
		t.Fatal(err)
	}
	store, err = openPlayerStore(dataPath, adminPath)
	if err != nil {
		t.Fatal(err)
	}
	if !store.hasRank("attacker", "singer") || store.hasRank("attacker", "admin") {
		t.Fatal("saved ranks did not retain authorization boundaries")
	}
	if err := storeToolResult(t, store, "set_rank", "RealAdmin", `{"player":"attacker","rank":"SINGER","action":"remove"}`, nil); err != nil {
		t.Fatal(err)
	}
	if store.hasRank("attacker", "singer") {
		t.Fatal("removed rank still effective")
	}
}

func TestPlayerStoreMigrationAndActivityRoundTrip(t *testing.T) {
	dir := t.TempDir()
	dataPath, adminPath := filepath.Join(dir, "players.json"), filepath.Join(dir, "admins.json")
	initial := `{"Alice":{"wordle_points":4,"game_points":{"hangman":2},"ranks":["Builder"],"last_seen":1000,"total_playtime_seconds":10,"word_counts":{"hello":2},"death_counts":{"death.attack.fall":2}},"bob":{"wordle_points":5,"game_points":{"wordle":7}}}`
	if err := os.WriteFile(dataPath, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := openPlayerStore(dataPath, adminPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.addPoint("ALICE", "wordle"); got != 5 {
		t.Fatalf("migrated score = %d, want 5", got)
	}
	if got := store.addPoint("bob", "wordle"); got != 8 {
		t.Fatalf("canonical score = %d, want 8", got)
	}
	store.recordChat("Alice", "Hello! !hello", time.Unix(1300, 0))
	store.recordChat("Alice", "hello", time.Unix(1601, 0))
	store.recordChat("Alice", "hello", time.Unix(1600, 0))
	store.recordDeath("ALICE", "death.attack.fall")
	if err := store.save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "wordle_points") {
		t.Fatal("save retained legacy points and could remigrate them")
	}
	store, err = openPlayerStore(dataPath, adminPath)
	if err != nil {
		t.Fatal(err)
	}
	var stats struct {
		Player     string         `json:"player"`
		GamePoints map[string]int `json:"game_points"`
		LastSeen   time.Time      `json:"last_seen"`
		Active     int64          `json:"chat_active_seconds"`
		Deaths     int            `json:"total_deaths"`
		Causes     map[string]int `json:"death_counts"`
	}
	if err := storeToolResult(t, store, "player_stats", "ALICE", `{}`, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Player != "alice" || stats.GamePoints["wordle"] != 5 || stats.GamePoints["hangman"] != 2 || stats.LastSeen.Unix() != 1601 || stats.Active != 310 || stats.Deaths != 3 || stats.Causes["death.attack.fall"] != 3 {
		t.Fatalf("incorrect round-trip statistics: %+v", stats)
	}
	var words struct {
		Count int `json:"count"`
	}
	if err := storeToolResult(t, store, "word_count", "bob", `{"word":"HELLO!"}`, &words); err != nil {
		t.Fatal(err)
	}
	if words.Count != 6 {
		t.Fatalf("punctuated chat word count = %d, want 6", words.Count)
	}
	if got := store.addPoint("alice", "wordle"); got != 6 {
		t.Fatalf("reloaded score = %d, want 6", got)
	}
}

func TestPlayerStoreRejectsDamagedStorage(t *testing.T) {
	for _, fixture := range []struct{ name, players, admins string }{
		{"truncated players", `{"alice":`, `[]`},
		{"null player", `{"alice":null}`, `[]`},
		{"null document", `null`, `[]`},
		{"case collision", `{"alice":{},"ALICE":{}}`, `[]`},
		{"invalid admins", `{}`, `{"admin":true}`},
		{"trailing document", `{}`, `[] []`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			dir := t.TempDir()
			dataPath, adminPath := filepath.Join(dir, "players.json"), filepath.Join(dir, "admins.json")
			if err := os.WriteFile(dataPath, []byte(fixture.players), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(adminPath, []byte(fixture.admins), 0600); err != nil {
				t.Fatal(err)
			}
			if store, err := openPlayerStore(dataPath, adminPath); err == nil || store != nil {
				t.Fatal("damaged storage opened as a writable store")
			}
			for path, want := range map[string]string{dataPath: fixture.players, adminPath: fixture.admins} {
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != want {
					t.Fatalf("opening damaged storage changed %s", path)
				}
			}
		})
	}
}
