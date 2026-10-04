// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// WipeVolumeCache removes only the UUID directory JuiceFS adds to cacheRoot.
// Recovery calls it before publishing metadata that may reuse slice IDs.
func WipeVolumeCache(cacheRoot, volumeUUID, stateDir string) error {
	if err := uuid.Validate(volumeUUID); err != nil {
		return fmt.Errorf("invalid cache volume UUID: %w", err)
	}
	if !filepath.IsAbs(cacheRoot) || !filepath.IsAbs(stateDir) {
		return errors.New("cache root and state directory must be absolute")
	}
	// JuiceFS cleans the joined path before opening the cache. Resolve only
	// its parent for the state overlap check; delete the joined path itself.
	path := filepath.Join(cacheRoot, volumeUUID)
	root, err := filepath.EvalSymlinks(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve cache root: %w", err)
	}
	state, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		return fmt.Errorf("resolve state directory: %w", err)
	}
	stateInfo, err := os.Stat(state)
	if err != nil {
		return fmt.Errorf("stat state directory: %w", err)
	}
	cacheInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat volume cache: %w", err)
	}
	if cacheInfo.IsDir() {
		checks := []struct {
			parent os.FileInfo
			child  string
		}{
			{cacheInfo, state},
			{stateInfo, filepath.Join(root, volumeUUID)},
		}
		for _, check := range checks {
			contains, err := directoryContains(check.parent, check.child)
			if err != nil {
				return fmt.Errorf("check cache and state overlap: %w", err)
			}
			if contains {
				return errors.New("volume cache overlaps the state directory; refusing to delete it")
			}
		}
	}
	// RemoveAll unlinks symlinks inside this directory without following them.
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove volume cache: %w", err)
	}
	return nil
}

func directoryContains(parent os.FileInfo, path string) (bool, error) {
	for {
		info, err := os.Stat(path)
		if err != nil {
			return false, err
		}
		if os.SameFile(parent, info) {
			return true, nil
		}
		next := filepath.Dir(path)
		if next == path {
			return false, nil
		}
		path = next
	}
}
