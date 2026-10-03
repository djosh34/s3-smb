// SPDX-License-Identifier: AGPL-3.0-only
package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	root, err := filepath.EvalSymlinks(cacheRoot)
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
	path := filepath.Join(root, volumeUUID)
	if withinDirectory(state, path) {
		return errors.New("volume cache overlaps the state directory; refusing to delete it")
	}
	// RemoveAll unlinks symlinks inside this directory without following them.
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove volume cache: %w", err)
	}
	return nil
}

func withinDirectory(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, string(os.PathSeparator))+string(os.PathSeparator))
}
