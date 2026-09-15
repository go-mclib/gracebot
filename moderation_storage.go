package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type actorBlock struct {
	Actor     string     `json:"actor"`
	By        string     `json:"by"`
	Reason    string     `json:"reason"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

func moderationActor(actor string) (string, error) {
	actor = normalizeStoreName(actor)
	if actor == discordBridgeActor {
		return actor, nil
	}
	return storePlayerName(actor)
}

// The caller supplies the operation's clock. Expiry is exclusive; creation is
// audit metadata, not a delayed activation time. No message text is stored here.
func (s *playerStore) setActorBlock(ctx context.Context, target, by, reason string, until *time.Time, at time.Time) (actorBlock, error) {
	target, err := moderationActor(target)
	if err != nil {
		return actorBlock{}, err
	}
	if by != "@automatic" {
		by, err = storageActor(by)
		if err != nil {
			return actorBlock{}, err
		}
	}
	if !validChatTime(at) || until != nil && (!validChatTime(*until) || !until.After(at)) {
		return actorBlock{}, errors.New("block expiry must follow creation")
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 500 {
		return actorBlock{}, errors.New("block reason exceeds 500 bytes")
	}
	var expiry any
	if until != nil {
		expiry = until.UnixNano()
	}
	// Serialize roster changes with presence observations, never call actorBlocked
	// while holding a database transaction (the store has one connection).
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return actorBlock{}, err
	}
	defer tx.Rollback()
	if by != "@automatic" {
		if err := s.requireAdminTx(ctx, tx, by); err != nil {
			return actorBlock{}, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO actor_blocks(actor,by_actor,reason,created_at,expires_at) VALUES(?,?,?,?,?)
ON CONFLICT(actor) DO UPDATE SET by_actor=excluded.by_actor,reason=excluded.reason,created_at=excluded.created_at,expires_at=excluded.expires_at
WHERE excluded.by_actor<>'@automatic' OR actor_blocks.expires_at<=? OR (actor_blocks.by_actor='@automatic' AND actor_blocks.expires_at IS NOT NULL AND (excluded.expires_at IS NULL OR excluded.expires_at>actor_blocks.expires_at))`, target, by, reason, at.UnixNano(), expiry, at.UnixNano())
	if err != nil {
		return actorBlock{}, err
	}
	block, err := scanActorBlock(tx.QueryRowContext(ctx, "SELECT actor,by_actor,reason,created_at,expires_at FROM actor_blocks WHERE actor=?", target))
	if err != nil {
		return actorBlock{}, err
	}
	if err = tx.Commit(); err != nil {
		return actorBlock{}, err
	}
	delete(s.online, target)
	return block, nil
}

func (s *playerStore) clearActorBlock(ctx context.Context, target, by string) (bool, error) {
	target, err := moderationActor(target)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := s.requireAdminTx(ctx, tx, by); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM actor_blocks WHERE actor=?", target)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n != 0, tx.Commit()
}

func scanActorBlock(row interface{ Scan(...any) error }) (actorBlock, error) {
	var block actorBlock
	var created int64
	var expires sql.NullInt64
	err := row.Scan(&block.Actor, &block.By, &block.Reason, &created, &expires)
	block.CreatedAt = time.Unix(0, created).UTC()
	if expires.Valid {
		t := time.Unix(0, expires.Int64).UTC()
		block.ExpiresAt = &t
	}
	return block, err
}

// notBlockedSQL only accepts internal column expressions, never user input.
// Callers bind a single timestamp after their existing query parameters.
func notBlockedSQL(columns ...string) string {
	matches := make([]string, len(columns))
	for i, column := range columns {
		matches[i] = "b.actor=lower(trim(" + column + "))"
	}
	return "NOT EXISTS (SELECT 1 FROM actor_blocks b WHERE (" + strings.Join(matches, " OR ") + ") AND (b.expires_at IS NULL OR b.expires_at>?))"
}

type blockQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func actorsBlocked(ctx context.Context, q blockQuerier, at time.Time, actors ...string) (bool, error) {
	columns := make([]string, len(actors))
	args := make([]any, 0, len(actors)+1)
	for i, actor := range actors {
		columns[i] = "?"
		args = append(args, actor)
	}
	args = append(args, at.UnixNano())
	var allowed bool
	err := q.QueryRowContext(ctx, "SELECT "+notBlockedSQL(columns...), args...).Scan(&allowed)
	return !allowed, err
}

func (s *playerStore) actorBlocked(actor string, at time.Time) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var blocked bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM actor_blocks WHERE actor=? AND (expires_at IS NULL OR expires_at>?))", normalizeStoreName(actor), at.UnixNano()).Scan(&blocked)
	return err != nil || blocked
}

func (s *playerStore) activeActorBlock(ctx context.Context, actor string, at time.Time) (*actorBlock, error) {
	block, err := scanActorBlock(s.db.QueryRowContext(ctx, "SELECT actor,by_actor,reason,created_at,expires_at FROM actor_blocks WHERE actor=? AND (expires_at IS NULL OR expires_at>?)", actor, at.UnixNano()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &block, nil
}

func (s *playerStore) blockedActors(ctx context.Context, at time.Time) ([]actorBlock, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT actor,by_actor,reason,created_at,expires_at FROM actor_blocks WHERE expires_at IS NULL OR expires_at>? ORDER BY actor", at.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	blocks := []actorBlock{}
	for rows.Next() {
		block, err := scanActorBlock(rows)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return blocks, rows.Err()
}

// Provenance remains after source events expire, so source exclusions must not
// depend on joining retained events.
var memoryNotBlockedSQL = notBlockedSQL("memories.owner", "memories.subject", "memories.attribution") +
	" AND NOT EXISTS (SELECT 1 FROM memory_sources ms JOIN actor_blocks b ON (b.actor=lower(trim(ms.owner)) OR b.actor=lower(trim(ms.sender))) WHERE ms.memory_id=memories.id AND (b.expires_at IS NULL OR b.expires_at>?))"

func (s *playerStore) migrateModerationStorage() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var displayColumn int
	if err = tx.QueryRow("SELECT count(*) FROM pragma_table_info('events') WHERE name='display_name'").Scan(&displayColumn); err != nil {
		return err
	}
	var legacyDiscord int
	if err = tx.QueryRow("SELECT count(*) FROM events WHERE (kind='discord' AND sender<>?) OR lower(substr(sender,1,9))='@discord:' OR lower(substr(owner,1,9))='@discord:'", discordBridgeActor).Scan(&legacyDiscord); err != nil {
		return err
	}
	if displayColumn == 0 {
		if _, err = tx.Exec("ALTER TABLE events ADD COLUMN display_name TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS actor_blocks(actor TEXT PRIMARY KEY,by_actor TEXT NOT NULL,reason TEXT NOT NULL,created_at INTEGER NOT NULL,expires_at INTEGER);
UPDATE events SET display_name=CASE WHEN display_name<>'' THEN display_name WHEN lower(substr(sender,1,9))='@discord:' THEN substr(sender,10) ELSE sender END,sender='#discord_bridge' WHERE kind='discord' AND sender<>'#discord_bridge';
UPDATE events SET sender='#discord_bridge' WHERE lower(substr(sender,1,9))='@discord:';
UPDATE events SET owner='#discord_bridge' WHERE lower(substr(owner,1,9))='@discord:';
UPDATE memory_sources SET sender='#discord_bridge' WHERE sender<>'#discord_bridge' AND (lower(substr(sender,1,9))='@discord:' OR event_id IN (SELECT id FROM events WHERE kind='discord'));
UPDATE memory_sources SET owner='#discord_bridge' WHERE lower(substr(owner,1,9))='@discord:';
UPDATE memories SET owner='#discord_bridge' WHERE lower(substr(owner,1,9))='@discord:';
UPDATE memories SET attribution='#discord_bridge' WHERE lower(substr(attribution,1,9))='@discord:';
UPDATE memories SET subject='#discord_bridge' WHERE lower(substr(subject,1,9))='@discord:';`); err != nil {
		return err
	}
	if displayColumn == 0 || legacyDiscord != 0 {
		var archives int
		if err = tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='archives'").Scan(&archives); err != nil {
			return err
		}
		if archives != 0 {
			// Force the next maintenance pass to replace pre-migration snapshots.
			if _, err = tx.Exec("UPDATE archives SET last_id=-1"); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
