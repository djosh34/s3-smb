// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// expiredSnapshots retains all snapshots for two days, then the earliest one
// per day through two weeks, per week through two months, and per 30-day period
// through two years. Points must be newest first, as returned by List.
func expiredSnapshots(points []Point, now time.Time) []string {
	cutoff := now.UTC().AddDate(-2, 0, 0)
	days := 2
	edge := now.UTC().AddDate(0, 0, -days)
	var expired []string
	var earliest string
	for _, p := range points {
		if p.Time.Before(cutoff) {
			expired = append(expired, p.Key)
			continue
		}
		if !p.Time.Before(edge) && days == 2 {
			continue
		}
		if p.Time.Before(edge) {
			earliest = ""
			for p.Time.Before(edge) {
				step := 30
				if days < 14 {
					step = 1
				} else if days < 60 {
					step = 7
				}
				days += step
				edge = edge.AddDate(0, 0, -step)
			}
		}
		if earliest != "" {
			expired = append(expired, earliest)
		}
		earliest = p.Key
	}
	return expired
}

func (m *Manager) cleanup(ctx context.Context, now time.Time) error {
	points, err := List(ctx, m.blob)
	if err != nil {
		return err
	}
	kept := make(map[string]bool, len(points))
	for _, p := range points {
		kept[filepath.Base(p.Key)] = true
	}
	store := guardedStore{m.blob, m.opts.Protection}
	for _, key := range expiredSnapshots(points, now) {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = store.Delete(ctx, key); err != nil {
			return err
		}
		delete(kept, filepath.Base(key))
	}
	// Keep recent failed/ambiguous names and every retained snapshot's name.
	// Older names can go once their remote snapshot has also been removed.
	dir := filepath.Join(m.opts.StateDir, "backup-names")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || kept[e.Name()] {
			continue
		}
		p, err := parsePoint("meta/" + e.Name())
		if err != nil || !p.Time.Before(now.UTC().AddDate(0, 0, -2)) {
			continue
		}
		if err = errors.Join(ctx.Err(), m.opts.Protection.Check()); err != nil {
			return err
		}
		if err = os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return syncDir(dir)
}

func cleanupSnapshotStaging(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "snapshot-") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		owned := true
		for _, f := range files {
			if !f.Type().IsRegular() || (f.Name() != "metadata.db" && f.Name() != "snapshot.db.gz") {
				owned = false
				break
			}
		}
		if owned {
			if err = os.RemoveAll(dir); err != nil {
				return err
			}
		}
	}
	return nil
}
