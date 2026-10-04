// SPDX-License-Identifier: AGPL-3.0-only
// Package helpers contains the platform-independent Mac harness checks.
package helpers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// Entry records a path's type and file content, not timestamps or permissions.
type Entry struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	SHA256 string `json:"sha256,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
}

// Counts summarizes the restored fixture.
type Counts struct {
	Entries int
	Files   int
	Bytes   int64
}

// Manifest hashes regular files without following symlinks.
func Manifest(root string) ([]Entry, Counts, error) {
	var rows []Entry
	var counts Counts
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		row := Entry{Path: filepath.ToSlash(relative), Type: "unexpected:" + d.Type().String()}
		switch {
		case d.IsDir():
			row.Type = "directory"
		case d.Type().IsRegular():
			file, err := os.Open(path) //nolint:gosec // WalkDir selected a regular file in the explicitly supplied fixture tree.
			if err != nil {
				return err
			}
			digest := sha256.New()
			size, readErr := io.Copy(digest, file)
			if err := errors.Join(readErr, file.Close()); err != nil {
				return err
			}
			row.Type, row.Bytes, row.SHA256 = "file", size, hex.EncodeToString(digest.Sum(nil))
			counts.Files++
			counts.Bytes += size
		}
		rows = append(rows, row)
		counts.Entries++
		return nil
	})
	return rows, counts, err
}

// WriteManifest writes a JSON reference with hashes only, refusing to overwrite it.
func WriteManifest(path string, rows []Entry) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // The caller chooses an exclusive run-owned output, not a user request.
	if err != nil {
		return err
	}
	return errors.Join(json.NewEncoder(file).Encode(rows), file.Close())
}

// ReadManifest reads one JSON reference. Compare validates empty and duplicate paths.
func ReadManifest(path string) ([]Entry, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The caller chooses a reference in the run-owned evidence or transfer directory.
	if err != nil {
		return nil, err
	}
	var rows []Entry
	err = json.Unmarshal(data, &rows)
	return rows, err
}

func index(rows []Entry) (map[string]Entry, error) {
	if len(rows) == 0 {
		return nil, errors.New("empty created-tree reference")
	}
	result := make(map[string]Entry, len(rows))
	for _, row := range rows {
		if _, exists := result[row.Path]; exists {
			return nil, fmt.Errorf("duplicate created-tree path %q", row.Path)
		}
		result[row.Path] = row
	}
	return result, nil
}

// Compare returns differing paths, ignoring row order but checking every type, size and hash.
func Compare(expected, actual []Entry) ([]string, error) {
	left, err := index(expected)
	if err != nil {
		return nil, err
	}
	right, err := index(actual)
	if err != nil {
		return nil, err
	}
	var paths []string
	for path, row := range left {
		if other, exists := right[path]; !exists || row != other {
			paths = append(paths, path)
		}
		delete(right, path)
	}
	for path := range right {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths, nil
}
