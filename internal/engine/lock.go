// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// lockPrefix holds one key per run: lock/<server ID>/<run ID>.
const lockPrefix = "lock/"

// loadServerID reads the random server ID kept in the data folder, creating
// it on first use. The same folder is the same server.
func loadServerID(dir string) (string, error) {
	path := filepath.Join(dir, serverIDName)
	data, err := os.ReadFile(filepath.Clean(path))
	if err == nil {
		id := strings.TrimSpace(string(data))
		if !validID(id) {
			return "", fmt.Errorf("%s does not hold a server ID", path)
		}
		return id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	temp := path + ".tmp"
	if err = writeSynced(temp, []byte(id+"\n")); err != nil {
		return "", err
	}
	if err = os.Rename(temp, path); err != nil {
		return "", err
	}
	return id, syncDir(dir)
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// takeLock puts this run's key, then lists every key. Another server's key
// is fresh when its Last-Modified is within the stale window of this run's
// own key in the same listing, so only the backend's clock counts. While one
// is fresh, it deletes its own key, waits and tries again. Deleting it lets
// two servers that start at once settle: whichever lists without seeing the
// other proceeds, and since each lists after its own PUT, at most one does.
// Keys of this server ID are earlier runs of this folder, which the folder
// lock says are gone.
func (e *Engine) takeLock(ctx context.Context, serverID string) error {
	for {
		sent := time.Now()
		if err := e.objs.put(ctx, e.lockKey, nil); err != nil {
			return fmt.Errorf("put the lock key: %w", err)
		}
		keys, err := e.objs.list(ctx, lockPrefix)
		if err != nil {
			return fmt.Errorf("list the lock keys: %w", err)
		}
		holder, err := freshHolder(keys, e.lockKey, serverID, e.tune.stale)
		if err != nil {
			return err
		}
		if holder == "" {
			e.timesMu.Lock()
			e.leaseUntil = sent.Add(e.tune.lease)
			e.timesMu.Unlock()
			return nil
		}
		e.log.Warn("another server holds the bucket; waiting until its lock goes stale", "lock", holder)
		if err = e.objs.remove(ctx, e.lockKey); err != nil {
			return fmt.Errorf("delete the lock key: %w", err)
		}
		// Half to one and a half times lockRetry, so two waiting servers
		// drift apart.
		wait := e.tune.lockRetry/2 + rand.N(e.tune.lockRetry+1) //nolint:gosec // Jitter needs no secure randomness.
		if !sleep(ctx, wait) {
			return ctx.Err()
		}
	}
}

// freshHolder returns the key of another server that is fresh against own.
func freshHolder(keys []object, own, serverID string, stale time.Duration) (string, error) {
	var mine *object
	for i := range keys {
		if keys[i].key == own {
			mine = &keys[i]
		}
	}
	if mine == nil {
		return "", fmt.Errorf("the lock listing does not show this run's key %s", own)
	}
	for _, k := range keys {
		if strings.HasPrefix(k.key, lockPrefix+serverID+"/") {
			continue
		}
		if mine.modified.Sub(k.modified) < stale {
			return k.key, nil
		}
	}
	return "", nil
}

// renewLoop puts the key every renewEvery. A success extends the lease to
// lease after that request was sent. A success that comes back after the
// lease ran out does not count: expiry is final.
func (e *Engine) renewLoop(ctx context.Context) {
	defer e.group.Done()
	e.timesMu.Lock()
	next := e.leaseUntil.Add(e.tune.renewEvery - e.tune.lease)
	e.timesMu.Unlock()
	for sleep(ctx, time.Until(next)) {
		sent := time.Now()
		next = sent.Add(e.tune.renewEvery)
		if err := e.put(ctx, e.lockKey, nil); err != nil {
			if e.Err() == nil {
				e.log.Warn("could not renew the bucket lock", "error", err)
			}
			continue
		}
		if err := e.check(); err != nil {
			return
		}
		e.timesMu.Lock()
		e.leaseUntil = sent.Add(e.tune.lease)
		e.timesMu.Unlock()
	}
}
