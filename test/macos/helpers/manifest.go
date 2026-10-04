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

// Entry describes content, not permissions or timestamps. References contain no file bytes.
type Entry struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	SHA256 string `json:"sha256,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
}

// Counts summarizes a fixture tree.
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

// WriteManifest writes one entry per line, refusing to overwrite evidence.
func WriteManifest(path string, rows []Entry) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // The caller chooses an exclusive evidence output, not a user request.
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			return errors.Join(err, file.Close())
		}
	}
	return file.Close()
}

// ReadManifest rejects empty or duplicate references.
func ReadManifest(path string) ([]Entry, error) {
	file, err := os.Open(path) //nolint:gosec // The caller chooses a manifest in the run-owned evidence or transfer directory.
	if err != nil {
		return nil, err
	}
	var rows []Entry
	decoder := json.NewDecoder(file)
	for {
		var row Entry
		err := decoder.Decode(&row)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.Join(err, file.Close())
		}
		rows = append(rows, row)
	}
	return rows, file.Close()
}

// Difference records missing, extra, or changed content.
type Difference struct {
	Expected *Entry `json:"expected"`
	Actual   *Entry `json:"actual"`
	Path     string `json:"path"`
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

// Compare ignores row order but checks every entry and its content.
func Compare(expected, actual []Entry) ([]Difference, error) {
	left, err := index(expected)
	if err != nil {
		return nil, err
	}
	right, err := index(actual)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(left)+len(right))
	for key := range left {
		keys = append(keys, key)
	}
	for key := range right {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var differences []Difference
	for _, key := range slices.Compact(keys) {
		l, lok := left[key]
		r, rok := right[key]
		if lok == rok && l == r {
			continue
		}
		difference := Difference{Path: key}
		if lok {
			difference.Expected = &l
		}
		if rok {
			difference.Actual = &r
		}
		differences = append(differences, difference)
	}
	return differences, nil
}
