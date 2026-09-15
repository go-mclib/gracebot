package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
)

const statisticsCoverage = "Retained chat, words, deaths and presence cover seven UTC calendar dates including today: at >= UTC midnight today minus six days. Statistics count only public player chat and deaths received while connected; private messages and command feedback never contribute, even for their owner. Presence intervals exclude disconnect and restart gaps; sessions are observations, not proven joins. No lifetime playtime or inferred chat-active time. Untimed legacy aggregates are preserved separately, excluded from timed statistics. Game scores, ranks and curated memories do not expire."

// Each heartbeat advances durable interval ends. A crash loses only time since
// the last observation; reopening never resumes an old interval.
func (s *playerStore) recordPresence(names []string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordPresenceLocked(names, at)
}
func (s *playerStore) recordPresenceLocked(names []string, at time.Time) error {
	if at.Before(s.presenceAt) {
		at = s.presenceAt
	}
	present := make(map[string]int64, len(names))
	for _, name := range names {
		name, err := storePlayerName(name)
		if err == nil {
			present[name] = s.online[name]
		}
	}
	// Presence is an observation, not a successful database write. Apply the
	// live roster even on failure so removed players can never resume an old
	// interval. Zero IDs mark observed players without a durable interval yet.
	previous := s.online
	s.online = present
	s.presenceAt = at
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for name := range present {
		blocked, err := actorsBlocked(context.Background(), tx, at, name)
		if err != nil {
			return err
		}
		if blocked {
			delete(present, name)
			delete(previous, name)
		}
	}
	next := make(map[string]int64, len(present))
	for name, id := range previous {
		if id == 0 {
			continue
		}
		blocked, err := actorsBlocked(context.Background(), tx, at, name)
		if err != nil {
			return err
		}
		if blocked {
			continue
		}
		if _, err = tx.Exec("UPDATE presence SET end=max(end,?) WHERE id=?", at.UnixNano(), id); err != nil {
			return err
		}
		if _, ok := present[name]; ok {
			next[name] = id
		}
	}
	for name := range present {
		if _, ok := next[name]; ok {
			continue
		}
		r, err := tx.Exec("INSERT INTO presence(player,start,end) VALUES(?,?,?)", name, at.UnixNano(), at.UnixNano())
		if err != nil {
			return err
		}
		id, err := r.LastInsertId()
		if err != nil {
			return err
		}
		next[name] = id
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.online = next
	return nil
}
func (s *playerStore) disconnectPresence(at time.Time) error { return s.recordPresence(nil, at) }
func (s *playerStore) recordDeath(sender, cause string) error {
	_, err := s.history().append("death", sender, cause, time.Now())
	return err
}

type retainedPlayerStats struct {
	Player        string           `json:"player"`
	Messages      int64            `json:"messages"`
	Words         int64            `json:"words"`
	Vocabulary    int              `json:"unique_vocabulary"`
	Deaths        int64            `json:"total_deaths"`
	DeathCounts   map[string]int64 `json:"death_counts"`
	GamePoints    map[string]int64 `json:"game_points"`
	FirstObserved *time.Time       `json:"first_observed"`
	LatestChat    *time.Time       `json:"latest_chat"`
	OnlineSeconds int64            `json:"observed_online_seconds"`
	Sessions      int64            `json:"observed_sessions"`
	Online        bool             `json:"observed_online"`
}

func (s *playerStore) retainedStats(ctx context.Context, actor string) (map[string]*retainedPlayerStats, error) {
	actor, err := storageActor(actor)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	cutoff := historyCutoff(now).UnixNano()
	s.mu.Lock()
	defer s.mu.Unlock()
	// One read transaction provides mutually consistent event/token views.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stats := map[string]*retainedPlayerStats{}
	get := func(name string) *retainedPlayerStats {
		p := stats[name]
		if p == nil {
			p = &retainedPlayerStats{Player: name, DeathCounts: map[string]int64{}, GamePoints: map[string]int64{}}
			stats[name] = p
		}
		return p
	}
	rows, err := tx.QueryContext(ctx, "SELECT sender,kind,count(*),min(at),max(at) FROM events WHERE at>=? AND owner='' AND kind IN ('player','death') AND "+notBlockedSQL("events.sender")+" GROUP BY sender,kind", cutoff, now.UnixNano())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name, kind string
		var count, first, last int64
		if err = rows.Scan(&name, &kind, &count, &first, &last); err != nil {
			rows.Close()
			return nil, err
		}
		p := get(name)
		f, l := time.Unix(0, first).UTC(), time.Unix(0, last).UTC()
		if p.FirstObserved == nil || f.Before(*p.FirstObserved) {
			p.FirstObserved = &f
		}
		if kind == "death" {
			p.Deaths += count
		} else {
			p.Messages += count
			if p.LatestChat == nil || l.After(*p.LatestChat) {
				p.LatestChat = &l
			}
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, "SELECT e.sender,sum(t.count),count(DISTINCT t.word) FROM tokens t JOIN events e ON e.id=t.event_id WHERE e.at>=? AND e.owner='' AND "+notBlockedSQL("e.sender")+" GROUP BY e.sender", cutoff, now.UnixNano())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name string
		var words int64
		var vocabulary int
		if err = rows.Scan(&name, &words, &vocabulary); err != nil {
			rows.Close()
			return nil, err
		}
		p := get(name)
		p.Words = words
		p.Vocabulary = vocabulary
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, "SELECT sender,text,count(*) FROM events WHERE at>=? AND owner='' AND kind='death' AND "+notBlockedSQL("events.sender")+" GROUP BY sender,text", cutoff, now.UnixNano())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name, cause string
		var count int64
		if err = rows.Scan(&name, &cause, &count); err != nil {
			rows.Close()
			return nil, err
		}
		get(name).DeathCounts[cause] = count
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, "SELECT player,game,points FROM scores WHERE "+notBlockedSQL("scores.player"), now.UnixNano())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name, game string
		var count int64
		if err = rows.Scan(&name, &game, &count); err != nil {
			rows.Close()
			return nil, err
		}
		get(name).GamePoints[game] = count
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, "SELECT id,player,start,end FROM presence WHERE end>=? AND "+notBlockedSQL("presence.player"), cutoff, now.UnixNano())
	if err != nil {
		return nil, err
	}
	nanos := map[string]int64{}
	for rows.Next() {
		var id, start, end int64
		var name string
		if err = rows.Scan(&id, &name, &start, &end); err != nil {
			rows.Close()
			return nil, err
		}
		p := get(name)
		if s.online[name] == id {
			end = max(end, now.UnixNano())
		}
		start = max(start, cutoff)
		nanos[name] += max(int64(0), end-start)
		p.Sessions++
		first := time.Unix(0, start).UTC()
		if p.FirstObserved == nil || first.Before(*p.FirstObserved) {
			p.FirstObserved = &first
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for name := range s.online {
		blocked, err := actorsBlocked(ctx, tx, now, name)
		if err != nil {
			return nil, err
		}
		if blocked {
			continue
		}
		get(name).Online = true
	}
	for name, n := range nanos {
		get(name).OnlineSeconds = n / int64(time.Second)
	}
	return stats, tx.Commit()
}
func (s *playerStore) playerStatsTool(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	var a struct {
		Player *string `json:"player"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	name := actor
	if a.Player != nil {
		name = *a.Player
	}
	name, err := storePlayerName(name)
	if err != nil {
		return nil, err
	}
	if s.actorBlocked(name, time.Now()) {
		return nil, errors.New("player unavailable")
	}
	stats, err := s.retainedStats(ctx, actor)
	if err != nil {
		return nil, err
	}
	p, exists := stats[name]
	if p == nil {
		p = &retainedPlayerStats{Player: name, GamePoints: map[string]int64{}, DeathCounts: map[string]int64{}}
	}
	ranks, err := s.playerRanks(ctx, name)
	if err != nil {
		return nil, err
	}
	return struct {
		*retainedPlayerStats
		Ranks         []string  `json:"ranks"`
		Recorded      bool      `json:"recorded"`
		Coverage      string    `json:"coverage"`
		RetainedSince time.Time `json:"retained_since"`
	}{p, ranks, exists || len(ranks) > 1, statisticsCoverage, historyCutoff(time.Now())}, nil
}
func (s *playerStore) leaderboardTool(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	var a struct {
		Metric string  `json:"metric"`
		Game   *string `json:"game"`
		Limit  int     `json:"limit"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.Limit == 0 {
		a.Limit = 10
	}
	if a.Limit < 1 || a.Limit > 25 || !slices.Contains([]string{"game", "messages", "words", "deaths", "observed_online_seconds"}, a.Metric) {
		return nil, errors.New("invalid leaderboard bounds or metric")
	}
	game := ""
	if a.Metric == "game" {
		if a.Game == nil {
			return nil, errors.New("game required")
		}
		game = normalizeStoreName(*a.Game)
		if !validStoreIdentifier(game, 64, true) {
			return nil, errors.New("invalid game")
		}
	} else if a.Game != nil {
		return nil, errors.New("game only applies to game metric")
	}
	stats, err := s.retainedStats(ctx, actor)
	if err != nil {
		return nil, err
	}
	type entry struct {
		Rank   int    `json:"rank"`
		Player string `json:"player"`
		Count  int64  `json:"count"`
	}
	entries := []entry{}
	for name, p := range stats {
		var n int64
		switch a.Metric {
		case "game":
			n = p.GamePoints[game]
		case "messages":
			n = p.Messages
		case "words":
			n = p.Words
		case "deaths":
			n = p.Deaths
		case "observed_online_seconds":
			n = p.OnlineSeconds
		}
		if n > 0 {
			entries = append(entries, entry{Player: name, Count: n})
		}
	}
	slices.SortFunc(entries, func(a, b entry) int {
		if a.Count > b.Count {
			return -1
		}
		if a.Count < b.Count {
			return 1
		}
		return strings.Compare(a.Player, b.Player)
	})
	if len(entries) > a.Limit {
		entries = entries[:a.Limit]
	}
	for i := range entries {
		entries[i].Rank = i + 1
	}
	return map[string]any{"metric": a.Metric, "game": game, "entries": entries, "coverage": statisticsCoverage}, nil
}
func (s *playerStore) wordCountTool(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	var a struct {
		Word   string `json:"word"`
		Player string `json:"player"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if len(a.Word) > 256 || len(strings.Fields(a.Word)) != 1 {
		return nil, errors.New("word must be one word of at most 256 bytes")
	}
	word := normalizeCountWord(a.Word)
	if word == "" {
		return nil, errors.New("word needs letters or digits")
	}
	actor, err := storageActor(actor)
	if err != nil {
		return nil, err
	}
	if a.Player != "" {
		a.Player, err = storePlayerName(a.Player)
		if err != nil {
			return nil, err
		}
	}
	var count int64
	err = s.db.QueryRowContext(ctx, "SELECT coalesce(sum(t.count),0) FROM tokens t JOIN events e ON e.id=t.event_id WHERE e.at>=? AND e.owner='' AND t.word=? AND (?='' OR e.sender=?) AND "+notBlockedSQL("e.sender"), historyCutoff(time.Now()).UnixNano(), word, a.Player, a.Player, time.Now().UnixNano()).Scan(&count)
	return map[string]any{"word": word, "player": a.Player, "global": a.Player == "", "count": count, "coverage": statisticsCoverage}, err
}
func (s *playerStore) topWordsTool(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	var a struct {
		Player string `json:"player"`
		Limit  int    `json:"limit"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.Limit == 0 {
		a.Limit = 10
	}
	if a.Limit < 1 || a.Limit > 25 {
		return nil, errors.New("limit must be 1–25")
	}
	actor, err := storageActor(actor)
	if err != nil {
		return nil, err
	}
	if a.Player != "" {
		a.Player, err = storePlayerName(a.Player)
		if err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, "SELECT t.word,sum(t.count) n FROM tokens t JOIN events e ON e.id=t.event_id WHERE e.at>=? AND e.owner='' AND (?='' OR e.sender=?) AND "+notBlockedSQL("e.sender")+" GROUP BY t.word ORDER BY n DESC,t.word LIMIT ?", historyCutoff(time.Now()).UnixNano(), a.Player, a.Player, time.Now().UnixNano(), a.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type entry struct {
		Word  string `json:"word"`
		Count int64  `json:"count"`
	}
	entries := []entry{}
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.Word, &e.Count); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return map[string]any{"player": a.Player, "global": a.Player == "", "entries": entries, "coverage": statisticsCoverage}, rows.Err()
}
func (s *playerStore) serverStatsTool(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	stats, err := s.retainedStats(ctx, actor)
	if err != nil {
		return nil, err
	}
	var messages, words, deaths, online, sessions, active int64
	for _, p := range stats {
		messages += p.Messages
		words += p.Words
		deaths += p.Deaths
		online += p.OnlineSeconds
		sessions += p.Sessions
		if p.Online {
			active++
		}
	}
	return map[string]any{"recorded_players": len(stats), "messages": messages, "words": words, "deaths": deaths, "observed_online_seconds": online, "observed_sessions": sessions, "active_observed_sessions": active, "retained_since": historyCutoff(time.Now()), "coverage": statisticsCoverage}, nil
}
