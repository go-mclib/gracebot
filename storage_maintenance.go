package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maintenance retains seven UTC calendar days including today. Completed days
// are private gzip JSONL snapshots, atomically replaced on late arrivals. Archive
// names are exclusively gracebot-chat-YYYY-MM-DD.jsonl.gz; unrelated files are
// untouched. Expired archives and event/presence rows use the identical cutoff.
// Ranks, scores, provenance and curated facts never participate in this deletion.
func (s *playerStore) maintenance(ctx context.Context, now time.Time, archiveDir string) error {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	if strings.TrimSpace(archiveDir) == "" {
		return errors.New("archive directory required")
	}
	if err := os.MkdirAll(archiveDir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(archiveDir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("archive directory must be a real directory")
	}
	if err = os.Chmod(archiveDir, 0700); err != nil {
		return err
	}
	s.mu.Lock()
	names := make([]string, 0, len(s.online))
	for name := range s.online {
		names = append(names, name)
	}
	err = s.recordPresenceLocked(names, now)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if _, err = s.db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS archives(day TEXT PRIMARY KEY,last_id INTEGER NOT NULL,row_count INTEGER NOT NULL)"); err != nil {
		return err
	}
	cutoff, today := historyCutoff(now), now.UTC().Truncate(24*time.Hour)
	for day := cutoff; day.Before(today); day = day.AddDate(0, 0, 1) {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = s.archiveDay(ctx, day, archiveDir); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".gracebot-archive-") {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.ModTime().Before(cutoff) {
				if err = os.Remove(filepath.Join(archiveDir, name)); err != nil {
					return err
				}
			}
			continue
		}
		if !strings.HasPrefix(name, "gracebot-chat-") || !strings.HasSuffix(name, ".jsonl.gz") {
			continue
		}
		date := strings.TrimSuffix(strings.TrimPrefix(name, "gracebot-chat-"), ".jsonl.gz")
		day, err := time.Parse("2006-01-02", date)
		if err != nil {
			continue
		}
		if day.Before(cutoff) {
			if err = os.Remove(filepath.Join(archiveDir, name)); err != nil {
				return err
			}
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{"DELETE FROM events WHERE at<?", "DELETE FROM presence WHERE end<?"} {
		if _, err = tx.ExecContext(ctx, q, cutoff.UnixNano()); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE presence SET start=? WHERE start<?", cutoff.UnixNano(), cutoff.UnixNano()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM archives WHERE day<?", cutoff.Format("2006-01-02")); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = s.checkpoint(ctx); err != nil {
		return err
	}
	// Reclaim at most 256 pages per call; never run a blocking full VACUUM.
	if _, err = s.db.ExecContext(ctx, "PRAGMA incremental_vacuum(256)"); err != nil {
		return err
	}
	return s.checkpoint(ctx)
}
func (s *playerStore) archiveDay(ctx context.Context, day time.Time, dir string) (resultErr error) {
	date := day.Format("2006-01-02")
	path := filepath.Join(dir, "gracebot-chat-"+date+".jsonl.gz")
	// A read transaction pins the same event set for the signature and archive.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var last, count int64
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(max(id),0),count(*) FROM events WHERE at>=? AND at<?", day.UnixNano(), day.AddDate(0, 0, 1).UnixNano()).Scan(&last, &count); err != nil {
		return err
	}
	var oldLast, oldCount int64
	if tx.QueryRowContext(ctx, "SELECT last_id,row_count FROM archives WHERE day=?", date).Scan(&oldLast, &oldCount) == nil && oldLast == last && oldCount == count {
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
			if err = os.Chmod(path, 0600); err != nil {
				return err
			}
			return tx.Commit()
		}
	}
	file, err := os.CreateTemp(dir, ".gracebot-archive-*")
	if err != nil {
		return err
	}
	closed, renamed := false, false
	defer func() {
		if !closed {
			resultErr = errors.Join(resultErr, file.Close())
		}
		if !renamed {
			resultErr = errors.Join(resultErr, os.Remove(file.Name()))
		}
	}()
	if err = file.Chmod(0600); err != nil {
		return err
	}
	zip := gzip.NewWriter(file)
	encoder := json.NewEncoder(zip)
	rows, err := tx.QueryContext(ctx, "SELECT "+eventColumns+" FROM events WHERE at>=? AND at<? ORDER BY id", day.UnixNano(), day.AddDate(0, 0, 1).UnixNano())
	if err != nil {
		zip.Close()
		return err
	}
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			break
		}
		var e chatEntry
		e, err = scanChatEntry(rows)
		if err != nil {
			break
		}
		if err = encoder.Encode(e); err != nil {
			break
		}
	}
	err = errors.Join(err, rows.Err(), rows.Close(), zip.Close())
	if err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	err = file.Close()
	closed = true
	if err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("publish archive: %w", err)
	}
	renamed = true
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = errors.Join(directory.Sync(), directory.Close())
	if err != nil {
		return err
	}
	// If a crash happens before this commit, the next pass safely rewrites the
	// completed atomic file. No event deletion depends on a partial archive.
	if _, err = tx.ExecContext(ctx, "INSERT INTO archives(day,last_id,row_count) VALUES(?,?,?) ON CONFLICT(day) DO UPDATE SET last_id=excluded.last_id,row_count=excluded.row_count", date, last, count); err != nil {
		return err
	}
	return tx.Commit()
}
