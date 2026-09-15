package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type legacyPlayerRecord struct {
	GamePoints           map[string]int64 `json:"game_points,omitempty"`
	Ranks                []string         `json:"ranks,omitempty"`
	WordlePoints         int64            `json:"wordle_points,omitempty"`
	FirstObserved        int64            `json:"first_observed,omitempty"`
	LastSeen             int64            `json:"last_seen,omitempty"`
	ChatActiveSeconds    int64            `json:"chat_active_seconds,omitempty"`
	TotalPlaytimeSeconds int64            `json:"total_playtime_seconds,omitempty"`
	Messages             int64            `json:"messages,omitempty"`
	MessagesSince        int64            `json:"messages_since,omitempty"`
	ObservedOnlineNanos  int64            `json:"observed_online_nanoseconds,omitempty"`
	ObservedSessions     int64            `json:"observed_sessions,omitempty"`
	PresenceSince        int64            `json:"presence_since,omitempty"`
	WordCounts           map[string]int64 `json:"word_counts,omitempty"`
	DeathCounts          map[string]int64 `json:"death_counts,omitempty"`
}

// The transaction includes both documents and import markers. A malformed final
// line rolls back all ranks, scores, events, tokens and markers. Source bytes
// are hashed on the actual read; a modified already-imported source is rejected
// rather than silently importing it twice. Originals are never opened writable.
func (s *playerStore) importLegacy(playerJSONPath, chatJSONLPath string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for index, path := range []string{playerJSONPath, chatJSONLPath} {
		if strings.TrimSpace(path) == "" {
			continue
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		source := fmt.Sprintf("%d:%s", index, absolute)
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		hash := sha256.New()
		if _, err = io.Copy(hash, f); err != nil {
			f.Close()
			return err
		}
		digest := hex.EncodeToString(hash.Sum(nil))
		var previous string
		err = tx.QueryRow("SELECT digest FROM imports WHERE source=?", source).Scan(&previous)
		if err == nil {
			f.Close()
			if previous != digest {
				return fmt.Errorf("legacy source changed after import: %s", path)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			f.Close()
			return err
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			f.Close()
			return err
		}
		hash.Reset()
		reader := io.TeeReader(f, hash)
		if index == 0 {
			err = importLegacyPlayers(tx, reader)
		} else {
			err = importLegacyChat(tx, reader)
		}
		closeErr := f.Close()
		if err != nil {
			return fmt.Errorf("import %s: %w", path, err)
		}
		if closeErr != nil {
			return closeErr
		}
		if digest != hex.EncodeToString(hash.Sum(nil)) {
			return errors.New("legacy source changed during import")
		}
		if _, err = tx.Exec("INSERT INTO imports(source,digest,imported_at) VALUES(?,?,?)", source, digest, time.Now().UTC().UnixNano()); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func importLegacyPlayers(tx *sql.Tx, reader io.Reader) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var players map[string]*legacyPlayerRecord
	if err := decoder.Decode(&players); err != nil {
		return err
	}
	if players == nil {
		return errors.New("null legacy player document")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("trailing legacy player data")
	}
	seen := map[string]bool{}
	for rawName, p := range players {
		name, err := storePlayerName(rawName)
		if err != nil {
			return err
		}
		if p == nil || seen[name] {
			return errors.New("null or duplicate normalized legacy player")
		}
		seen[name] = true
		blocked, err := actorsBlocked(context.Background(), tx, time.Now(), name)
		if err != nil {
			return err
		}
		if blocked {
			continue
		}
		for _, n := range []int64{p.WordlePoints, p.FirstObserved, p.LastSeen, p.ChatActiveSeconds, p.TotalPlaytimeSeconds, p.Messages, p.MessagesSince, p.ObservedOnlineNanos, p.ObservedSessions, p.PresenceSince} {
			if n < 0 {
				return errors.New("negative legacy statistic")
			}
		}
		for _, counts := range []map[string]int64{p.GamePoints, p.WordCounts, p.DeathCounts} {
			for label, count := range counts {
				if strings.TrimSpace(label) == "" || count < 0 {
					return errors.New("invalid legacy count")
				}
			}
		}
		document, err := json.Marshal(p)
		if err != nil {
			return err
		}
		// Untimed counters survive as explicitly untimed evidence, never event rows.
		if _, err = tx.Exec("INSERT INTO legacy_aggregates(player,document) VALUES(?,?) ON CONFLICT(player) DO NOTHING", name, string(document)); err != nil {
			return err
		}
		if p.GamePoints == nil {
			p.GamePoints = map[string]int64{}
		}
		if _, ok := p.GamePoints["wordle"]; !ok && p.WordlePoints > 0 {
			p.GamePoints["wordle"] = p.WordlePoints
		}
		games := map[string]bool{}
		for game, points := range p.GamePoints {
			game = normalizeStoreName(game)
			if !validStoreIdentifier(game, 64, true) || games[game] {
				return errors.New("invalid or duplicate legacy game")
			}
			games[game] = true
			if _, err = tx.Exec("INSERT INTO scores(player,game,points) VALUES(?,?,?) ON CONFLICT(player,game) DO UPDATE SET points=max(points,excluded.points)", name, game, points); err != nil {
				return err
			}
		}
		for _, rank := range p.Ranks {
			rank = normalizeStoreName(rank)
			if rank == "admin" || rank == "default" {
				continue
			}
			if !validStoreIdentifier(rank, 64, false) {
				return errors.New("invalid legacy rank")
			}
			if _, err = tx.Exec("INSERT OR IGNORE INTO ranks(player,rank) VALUES(?,?)", name, rank); err != nil {
				return err
			}
		}
	}
	return nil
}
func importLegacyChat(tx *sql.Tx, reader io.Reader) error {
	// ReadBytes is replaced by Scanner's bounded buffer: pathological lines cannot
	// allocate unbounded memory. Require the final newline to detect interrupted writes.
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxEventBytes*8)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return 0, nil, errors.New("incomplete final legacy chat record")
		}
		return 0, nil, nil
	})
	var last uint64
	for scanner.Scan() {
		var e chatEntry
		d := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		d.DisallowUnknownFields()
		if err := d.Decode(&e); err != nil {
			return err
		}
		var extra any
		if err := d.Decode(&extra); err != io.EOF {
			return errors.New("trailing legacy chat record data")
		}
		if e.ID != last+1 {
			return errors.New("nonsequential legacy event IDs")
		}
		last = e.ID
		if e.Kind == "discord" && strings.HasPrefix(normalizeStoreName(e.Sender), "@discord:") {
			e.DisplayName = e.Sender[len("@discord:"):]
			e.Sender = discordBridgeActor
		}
		if e.Kind == "private" && e.Owner == "" {
			e.Owner = e.Sender
		}
		// Legacy records lack the chat-type metadata needed to distinguish
		// public system/disguised chat from private command or plugin feedback.
		// Preserve explicit ownership, but never infer public origin from text.
		if e.Owner == "" && (e.Kind == "system" || e.Kind == "disguised") {
			e.Owner = "@unattributed"
		}
		if err := validateChatEntry(&e); err != nil {
			return err
		}
		sourceID := e.ID
		e.SourceID = &sourceID
		e.Source = "legacy_chat"
		if _, err := insertChatEntry(context.Background(), tx, e); err != nil {
			return err
		}
	}
	return scanner.Err()
}
