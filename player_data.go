package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

type playerRecord struct {
	GamePoints           map[string]int `json:"game_points,omitempty"`
	Ranks                []string       `json:"ranks,omitempty"`
	LastSeen             int64          `json:"last_seen,omitempty"`
	TotalPlaytimeSeconds int64          `json:"total_playtime_seconds,omitempty"`
	WordCounts           map[string]int `json:"word_counts,omitempty"`
	DeathCounts          map[string]int `json:"death_counts,omitempty"`
}

type playerStore struct {
	mu sync.RWMutex
	// Serialize the snapshot and replacement together so an older save cannot
	// replace a newer snapshot after concurrent callers finish out of order.
	saveMu   sync.Mutex
	players  map[string]*playerRecord
	admins   map[string]bool
	dataPath string
}

func openPlayerStore(dataPath, adminPath string) (*playerStore, error) {
	if strings.TrimSpace(dataPath) == "" || strings.TrimSpace(adminPath) == "" {
		return nil, errors.New("player and admin storage paths are required")
	}
	s := &playerStore{players: make(map[string]*playerRecord), admins: make(map[string]bool), dataPath: dataPath}
	var admins []string
	if err := readStoreJSON(adminPath, &admins); err != nil {
		return nil, err
	}
	for _, admin := range admins {
		admin = normalizeStoreName(admin)
		if admin == "" {
			return nil, fmt.Errorf("%s: empty admin name", adminPath)
		}
		s.admins[admin] = true
	}
	// Keep the legacy field confined to loading; saving always writes only the
	// current schema, so migration cannot be applied twice.
	var loaded map[string]*struct {
		playerRecord
		WordlePoints int `json:"wordle_points,omitempty"`
	}
	if err := readStoreJSON(dataPath, &loaded); err != nil {
		return nil, err
	}
	for name, record := range loaded {
		key := normalizeStoreName(name)
		if key == "" || record == nil {
			return nil, fmt.Errorf("%s: invalid player record %q", dataPath, name)
		}
		if _, exists := s.players[key]; exists {
			return nil, fmt.Errorf("%s: duplicate normalized player %q", dataPath, key)
		}
		if record.LastSeen < 0 || record.TotalPlaytimeSeconds < 0 || record.WordlePoints < 0 {
			return nil, fmt.Errorf("%s: negative player statistic for %q", dataPath, name)
		}
		for _, counts := range []map[string]int{record.GamePoints, record.WordCounts, record.DeathCounts} {
			for label, count := range counts {
				if label == "" || count < 0 {
					return nil, fmt.Errorf("%s: invalid count for player %q", dataPath, name)
				}
			}
		}
		if record.GamePoints == nil {
			record.GamePoints = make(map[string]int)
		}
		if _, exists := record.GamePoints["wordle"]; !exists && record.WordlePoints > 0 {
			record.GamePoints["wordle"] = record.WordlePoints
		}
		ranks := make([]string, 0, len(record.Ranks))
		for _, rank := range record.Ranks {
			rank = normalizeStoreName(rank)
			// Admin authority is exclusively the separately loaded admins file.
			if rank != "" && rank != "admin" && rank != "default" && !slices.Contains(ranks, rank) {
				ranks = append(ranks, rank)
			}
		}
		slices.Sort(ranks)
		record.Ranks = ranks
		s.players[key] = &record.playerRecord
	}
	return s, nil
}

func readStoreJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("decode %s: null is not a storage document", path)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func (s *playerStore) save() (err error) {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.RLock()
	data, err := json.MarshalIndent(s.players, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("encode player data: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(s.dataPath), ".player-data-*")
	if err != nil {
		return fmt.Errorf("create player data snapshot: %w", err)
	}
	closed, renamed := false, false
	defer func() {
		if !closed {
			err = errors.Join(err, file.Close())
		}
		if !renamed {
			err = errors.Join(err, os.Remove(file.Name()))
		}
	}()
	if _, err = file.Write(data); err != nil {
		return fmt.Errorf("write player data snapshot: %w", err)
	}
	if err = file.Sync(); err != nil {
		return fmt.Errorf("sync player data snapshot: %w", err)
	}
	err = file.Close()
	closed = true
	if err != nil {
		return fmt.Errorf("close player data snapshot: %w", err)
	}
	if err = os.Rename(file.Name(), s.dataPath); err != nil {
		return fmt.Errorf("replace player data: %w", err)
	}
	renamed = true
	return nil
}

func normalizeStoreName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// Callers hold mu for writing. No record pointer escapes the store.
func (s *playerStore) playerLocked(name string) *playerRecord {
	if s.players[name] == nil {
		s.players[name] = &playerRecord{}
	}
	return s.players[name]
}

func (s *playerStore) recordChat(sender, message string, at time.Time) {
	sender = normalizeStoreName(sender)
	s.mu.Lock()
	defer s.mu.Unlock()
	player := s.playerLocked(sender)
	now := at.Unix()
	if now > player.LastSeen {
		gap := now - player.LastSeen
		if player.LastSeen > 0 && gap <= 300 {
			player.TotalPlaytimeSeconds += gap
		}
		player.LastSeen = now
	}
	for _, word := range strings.Fields(message) {
		word = normalizeCountWord(word)
		if word != "" {
			if player.WordCounts == nil {
				player.WordCounts = make(map[string]int)
			}
			player.WordCounts[word]++
		}
	}
}

func normalizeCountWord(word string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, word)
}

func (s *playerStore) recordDeath(sender, cause string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	player := s.playerLocked(normalizeStoreName(sender))
	if player.DeathCounts == nil {
		player.DeathCounts = make(map[string]int)
	}
	player.DeathCounts[cause]++
}

func (s *playerStore) hasRank(sender, rank string) bool {
	sender, rank = normalizeStoreName(sender), normalizeStoreName(rank)
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch rank {
	case "default":
		return true
	case "admin":
		return s.admins[sender]
	default:
		player := s.players[sender]
		return player != nil && slices.Contains(player.Ranks, rank)
	}
}

func (s *playerStore) addPoint(sender, game string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	player := s.playerLocked(normalizeStoreName(sender))
	game = normalizeStoreName(game)
	if player.GamePoints == nil {
		player.GamePoints = make(map[string]int)
	}
	player.GamePoints[game]++
	return player.GamePoints[game]
}

func (s *playerStore) ranksLocked(name string) []string {
	ranks := []string{"default"}
	if s.admins[name] {
		ranks = append(ranks, "admin")
	}
	if player := s.players[name]; player != nil {
		ranks = append(ranks, player.Ranks...)
	}
	return ranks
}

func validStoreIdentifier(value string, maxLength int, underscore bool) bool {
	if len(value) == 0 || len(value) > maxLength {
		return false
	}
	for _, c := range value {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || (underscore && c == '_') {
			continue
		}
		return false
	}
	return true
}

func storePlayerName(value string) (string, error) {
	value = normalizeStoreName(value)
	if !validStoreIdentifier(value, 16, true) {
		return "", errors.New("player must be a Minecraft username (1–16 letters, digits or underscores)")
	}
	return value, nil
}

func (s *playerStore) tools() []toolSpec {
	return []toolSpec{
		makeTool("player_stats", "Get a player's ranks, game points, last seen, approximate chat-active seconds (not online playtime), and death counts. Defaults to the requesting player.", map[string]any{
			"player": map[string]any{"type": "string", "description": "Minecraft username; omit for the requester"},
		}, nil, s.playerStatsTool),
		makeTool("leaderboard", "Rank players by game points or total deaths, highest first; ties sort by username.", map[string]any{
			"metric": map[string]any{"type": "string", "enum": []string{"game", "deaths"}},
			"game":   map[string]any{"type": "string", "description": "Required for game metric, e.g. wordle or hangman; omit for deaths"},
			"limit":  map[string]any{"type": "integer", "minimum": 1, "maximum": 25, "description": "Maximum entries; defaults to 10"},
		}, []string{"metric"}, s.leaderboardTool),
		makeTool("word_count", "Count a word in recorded chat, case-insensitive and ignoring punctuation. Omit player to count globally.", map[string]any{
			"word":   map[string]any{"type": "string"},
			"player": map[string]any{"type": "string", "description": "Minecraft username; omit for all players"},
		}, []string{"word"}, s.wordCountTool),
		makeTool("set_rank", "Add or remove a custom player rank. Only an administrator from the admins file may do this; admin and default ranks cannot be changed.", map[string]any{
			"player": map[string]any{"type": "string"},
			"rank":   map[string]any{"type": "string", "description": "1–64 letters or digits, excluding admin and default"},
			"action": map[string]any{"type": "string", "enum": []string{"add", "remove"}},
		}, []string{"player", "rank", "action"}, s.setRankTool),
	}
}

func (s *playerStore) playerStatsTool(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	var args struct {
		Player *string `json:"player"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := actor
	if args.Player != nil {
		name = *args.Player
	}
	name, err := storePlayerName(name)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	player, exists := s.players[name]
	if !exists {
		player = &playerRecord{}
	}
	deaths := 0
	for _, count := range player.DeathCounts {
		deaths += count
	}
	var lastSeen *time.Time
	if player.LastSeen > 0 {
		at := time.Unix(player.LastSeen, 0).UTC()
		lastSeen = &at
	}
	return map[string]any{
		"player": name, "recorded": exists, "ranks": s.ranksLocked(name),
		"game_points": maps.Clone(player.GamePoints), "last_seen": lastSeen,
		"chat_active_seconds": player.TotalPlaytimeSeconds,
		"total_deaths":        deaths, "death_counts": maps.Clone(player.DeathCounts),
	}, nil
}

func (s *playerStore) leaderboardTool(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
	var args struct {
		Metric string  `json:"metric"`
		Game   *string `json:"game"`
		Limit  *int    `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if args.Metric != "game" && args.Metric != "deaths" {
		return nil, errors.New("metric must be game or deaths")
	}
	game := ""
	if args.Metric == "game" {
		if args.Game == nil {
			return nil, errors.New("game is required for game leaderboard")
		}
		game = normalizeStoreName(*args.Game)
		if !validStoreIdentifier(game, 64, true) {
			return nil, errors.New("invalid game name")
		}
	} else if args.Game != nil {
		return nil, errors.New("game must be omitted for death leaderboard")
	}
	limit := 10
	if args.Limit != nil {
		limit = *args.Limit
	}
	if limit < 1 || limit > 25 {
		return nil, errors.New("limit must be between 1 and 25")
	}
	type entry struct {
		Rank   int    `json:"rank"`
		Player string `json:"player"`
		Count  int    `json:"count"`
	}
	entries := make([]entry, 0)
	s.mu.RLock()
	for name, player := range s.players {
		count := player.GamePoints[game]
		if args.Metric == "deaths" {
			count = 0
			for _, deaths := range player.DeathCounts {
				count += deaths
			}
		}
		if count > 0 {
			entries = append(entries, entry{Player: name, Count: count})
		}
	}
	s.mu.RUnlock()
	slices.SortFunc(entries, func(a, b entry) int {
		if a.Count > b.Count {
			return -1
		}
		if a.Count < b.Count {
			return 1
		}
		return strings.Compare(a.Player, b.Player)
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	for index := range entries {
		entries[index].Rank = index + 1
	}
	return map[string]any{"metric": args.Metric, "game": game, "entries": entries}, nil
}

func (s *playerStore) wordCountTool(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
	var args struct {
		Word   string  `json:"word"`
		Player *string `json:"player"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(args.Word) > 256 || len(strings.Fields(args.Word)) != 1 {
		return nil, errors.New("word must be one word of at most 256 bytes")
	}
	word := normalizeCountWord(args.Word)
	if word == "" {
		return nil, errors.New("word must contain letters or digits")
	}
	name := ""
	if args.Player != nil {
		var err error
		name, err = storePlayerName(*args.Player)
		if err != nil {
			return nil, err
		}
	}
	count := 0
	s.mu.RLock()
	if name == "" {
		for _, player := range s.players {
			count += player.WordCounts[word]
		}
	} else if player := s.players[name]; player != nil {
		count = player.WordCounts[word]
	}
	s.mu.RUnlock()
	return map[string]any{"word": word, "player": name, "global": name == "", "count": count}, nil
}

func (s *playerStore) setRankTool(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	var args struct {
		Player string `json:"player"`
		Rank   string `json:"rank"`
		Action string `json:"action"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := storePlayerName(args.Player)
	if err != nil {
		return nil, err
	}
	rank := normalizeStoreName(args.Rank)
	if !validStoreIdentifier(rank, 64, false) {
		return nil, errors.New("rank must contain 1–64 letters or digits")
	}
	if rank == "admin" || rank == "default" {
		return nil, errors.New("reserved ranks cannot be changed")
	}
	if args.Action != "add" && args.Action != "remove" {
		return nil, errors.New("action must be add or remove")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.admins[normalizeStoreName(actor)] {
		return nil, errors.New("only an administrator from the admins file may change ranks")
	}
	player := s.players[name]
	index := -1
	if player != nil {
		index = slices.Index(player.Ranks, rank)
	}
	if args.Action == "add" {
		if index >= 0 {
			return nil, errors.New("player already has this rank")
		}
		player = s.playerLocked(name)
		player.Ranks = append(player.Ranks, rank)
		slices.Sort(player.Ranks)
	} else {
		if index < 0 {
			return nil, errors.New("player does not have this rank")
		}
		player.Ranks = slices.Delete(player.Ranks, index, index+1)
	}
	return map[string]any{"player": name, "ranks": s.ranksLocked(name), "action": args.Action, "rank": rank}, nil
}
