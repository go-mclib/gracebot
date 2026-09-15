package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

// All durable state has one transactional owner. online only identifies live
// observation intervals; it is never restored across a process restart.
type playerStore struct {
	db            *sql.DB
	admins        map[string]bool
	mu            sync.Mutex
	online        map[string]int64
	presenceAt    time.Time
	maintenanceMu sync.Mutex
}

func openPlayerStore(databasePath, adminPath string) (*playerStore, error) {
	if strings.TrimSpace(databasePath) == "" || strings.TrimSpace(adminPath) == "" {
		return nil, errors.New("database and admin paths are required")
	}
	absolute, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, err
	}
	databasePath = absolute
	if info, err := os.Lstat(databasePath); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("database must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var admins []string
	if err := readStoreJSON(adminPath, &admins); err != nil {
		return nil, err
	}
	s := &playerStore{admins: map[string]bool{}, online: map[string]int64{}}
	for _, name := range admins {
		name, err := storePlayerName(name)
		if err != nil {
			return nil, fmt.Errorf("admin name: %w", err)
		}
		s.admins[name] = true
	}
	dir := filepath.Dir(databasePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// Never chmod an existing parent directory: it may contain unrelated user data.
	f, err := os.OpenFile(databasePath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	err = errors.Join(f.Chmod(0600), f.Close())
	if err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: databasePath}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s.db = db
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON; PRAGMA auto_vacuum=INCREMENTAL; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA wal_autocheckpoint=256; PRAGMA journal_size_limit=4194304;
 CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, kind TEXT NOT NULL, sender TEXT NOT NULL, owner TEXT NOT NULL DEFAULT '', text TEXT NOT NULL, source TEXT NOT NULL DEFAULT 'live', source_id INTEGER);
 CREATE INDEX IF NOT EXISTS events_time ON events(at,id);
 CREATE INDEX IF NOT EXISTS events_actor ON events(sender,at,id);
 CREATE INDEX IF NOT EXISTS events_owner ON events(owner,id);
 CREATE TABLE IF NOT EXISTS tokens(event_id INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE, word TEXT NOT NULL, count INTEGER NOT NULL, PRIMARY KEY(event_id,word));
 CREATE INDEX IF NOT EXISTS tokens_word ON tokens(word,event_id);
 CREATE TABLE IF NOT EXISTS scores(player TEXT NOT NULL, game TEXT NOT NULL, points INTEGER NOT NULL CHECK(points>=0), PRIMARY KEY(player,game));
 CREATE TABLE IF NOT EXISTS ranks(player TEXT NOT NULL, rank TEXT NOT NULL, PRIMARY KEY(player,rank));
 CREATE TABLE IF NOT EXISTS presence(id INTEGER PRIMARY KEY, player TEXT NOT NULL, start INTEGER NOT NULL, end INTEGER NOT NULL CHECK(end>=start));
 CREATE INDEX IF NOT EXISTS presence_player ON presence(player,start);
 CREATE TABLE IF NOT EXISTS imports(source TEXT PRIMARY KEY, digest TEXT NOT NULL, imported_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS legacy_aggregates(player TEXT PRIMARY KEY, document TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS memories(id INTEGER PRIMARY KEY AUTOINCREMENT, subject TEXT NOT NULL, fact TEXT NOT NULL, owner TEXT NOT NULL, visibility TEXT NOT NULL CHECK(visibility IN ('public','private')), attribution TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS memories_subject ON memories(subject,id);
 CREATE TABLE IF NOT EXISTS memory_sources(memory_id INTEGER NOT NULL REFERENCES memories(id) ON DELETE CASCADE, event_id INTEGER NOT NULL, owner TEXT NOT NULL, sender TEXT NOT NULL, at INTEGER NOT NULL, PRIMARY KEY(memory_id,event_id));`); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.migrateModerationStorage(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func readStoreJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("null is not a storage document")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return errors.New("trailing storage data")
	}
	return nil
}

func (s *playerStore) history() *chatHistory { return &chatHistory{store: s} }
func (s *playerStore) checkpoint(ctx context.Context) error {
	var busy, logFrames, checkpointed int
	if err := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("SQLite checkpoint busy")
	}
	return nil
}
func (s *playerStore) close() error {
	return errors.Join(s.disconnectPresence(time.Now()), s.checkpoint(context.Background()), s.db.Close())
}

func normalizeStoreName(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
func normalizeCountWord(word string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, word)
}
func validStoreIdentifier(value string, maxLength int, underscore bool) bool {
	if len(value) == 0 || len(value) > maxLength {
		return false
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || underscore && c == '_' {
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
func storageActor(value string) (string, error) {
	if value == consoleActor || value == "@server" || value == discordBridgeActor {
		return value, nil
	}
	return storePlayerName(value)
}
func (s *playerStore) hasRank(sender, rank string) bool {
	sender, rank = normalizeStoreName(sender), normalizeStoreName(rank)
	if rank == "default" {
		return true
	}
	if sender == discordBridgeActor || strings.HasPrefix(sender, "@discord:") {
		return false
	}
	if rank == "admin" && (sender == consoleActor || s.admins[sender]) {
		return true
	}
	var found int
	return s.db.QueryRow("SELECT 1 FROM ranks WHERE player=? AND rank=?", sender, rank).Scan(&found) == nil
}

// requireAdminTx checks authority in the same transaction as a durable mutation.
func (s *playerStore) requireAdminTx(ctx context.Context, tx *sql.Tx, actor string) error {
	actor = normalizeStoreName(actor)
	if actor == consoleActor || s.admins[actor] {
		return nil
	}
	if !validUsername(actor) {
		return errors.New("administrator authority required")
	}
	var authorized int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM ranks WHERE player=? AND rank='admin'", actor).Scan(&authorized)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("administrator authority required")
	}
	return err
}
func (s *playerStore) addPoint(sender, game string) int {
	sender, err := storePlayerName(sender)
	game = normalizeStoreName(game)
	if err != nil || !validStoreIdentifier(game, 64, true) {
		log.Printf("invalid game score attribution")
		return 0
	}
	var points int
	err = s.db.QueryRow("INSERT INTO scores(player,game,points) SELECT ?,?,1 WHERE "+notBlockedSQL("?")+" ON CONFLICT(player,game) DO UPDATE SET points=points+1 RETURNING points", sender, game, sender, time.Now().UnixNano()).Scan(&points)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		log.Printf("persist game score: %v", err)
		return 0
	}
	return points
}
func (s *playerStore) playerRanks(ctx context.Context, name string) ([]string, error) {
	result := []string{"default"}
	if s.hasRank(name, "admin") {
		result = append(result, "admin")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT rank FROM ranks WHERE player=? AND rank NOT IN ('admin','default') ORDER BY rank", name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var rank string
		if err = rows.Scan(&rank); err != nil {
			return nil, err
		}
		result = append(result, rank)
	}
	return result, rows.Err()
}
func (s *playerStore) setRankTool(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	var a struct {
		Player string `json:"player"`
		Rank   string `json:"rank"`
		Action string `json:"action"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if !s.hasRank(actor, "admin") {
		return nil, errors.New("administrator authority required")
	}
	name, err := storePlayerName(a.Player)
	if err != nil {
		return nil, err
	}
	rank := normalizeStoreName(a.Rank)
	if !validStoreIdentifier(rank, 64, false) || rank == "default" {
		return nil, errors.New("invalid or reserved rank")
	}
	if rank == "admin" && s.admins[name] {
		return nil, errors.New("bootstrap administrators are managed in admins.json")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.requireAdminTx(ctx, tx, actor); err != nil {
		return nil, err
	}
	var result sql.Result
	switch a.Action {
	case "add":
		result, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO ranks(player,rank) VALUES(?,?)", name, rank)
	case "remove":
		result, err = tx.ExecContext(ctx, "DELETE FROM ranks WHERE player=? AND rank=?", name, rank)
	default:
		return nil, errors.New("action must be add or remove")
	}
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, errors.New("rank already in requested state")
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	ranks, err := s.playerRanks(ctx, name)
	return map[string]any{"player": name, "ranks": ranks, "action": a.Action, "rank": rank}, err
}
func (s *playerStore) tools() []toolSpec {
	str := map[string]any{"type": "string"}
	limit := map[string]any{"type": "integer", "minimum": 1, "maximum": 25}
	tools := []toolSpec{
		makeTool("player_stats", "Statistics derived from retained public player chat, deaths and observed presence; games and ranks persist. Seven UTC calendar dates including today, not lifetime playtime.", map[string]any{"player": str}, nil, s.playerStatsTool),
		makeTool("leaderboard", "Rank game scores or retained public messages, words, deaths and observed online seconds.", map[string]any{"metric": map[string]any{"type": "string", "enum": []string{"game", "messages", "words", "deaths", "observed_online_seconds"}}, "game": str, "limit": limit}, []string{"metric"}, s.leaderboardTool),
		makeTool("word_count", "Count a normalized word in retained public player chat; private messages never contribute.", map[string]any{"word": str, "player": str}, []string{"word"}, s.wordCountTool),
		makeTool("top_words", "Most frequent normalized words in retained public player chat; private messages never contribute.", map[string]any{"player": str, "limit": limit}, nil, s.topWordsTool),
		makeTool("server_stats", "Aggregate retained public events and observed presence, not lifetime server statistics.", map[string]any{}, nil, s.serverStatsTool),
		makeTool("set_rank", "Administrator-only rank change, including admin grants. The default rank and admins.json bootstrap administrators cannot be changed.", map[string]any{"player": str, "rank": str, "action": map[string]any{"type": "string", "enum": []string{"add", "remove"}}}, []string{"player", "rank", "action"}, s.setRankTool),
	}
	return append(tools, s.memoryTools()...)
}
