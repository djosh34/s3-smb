// SPDX-License-Identifier: AGPL-3.0-only
// Package helpers holds the parsers and pass or fail checks of the Mac
// harness that do not need a Mac, so they can be tested on Linux.
package helpers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
)

// Entry records a path's type and file content, not timestamps or permissions.
type Entry struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	SHA256 string `json:"sha256,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
}

// Counts summarizes a manifest.
type Counts struct {
	Entries int
	Files   int
	Bytes   int64
}

// Manifest hashes the regular files under tree without following symlinks.
func Manifest(tree string) ([]Entry, Counts, error) {
	root, err := os.OpenRoot(tree)
	if err != nil {
		return nil, Counts{}, err
	}
	var rows []Entry
	var counts Counts
	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		row := Entry{Path: path, Type: "unexpected:" + d.Type().String()}
		switch {
		case d.IsDir():
			row.Type = "directory"
		case d.Type().IsRegular():
			size, sum, hashErr := hashFile(root, path)
			if hashErr != nil {
				return hashErr
			}
			row.Type, row.Bytes, row.SHA256 = "file", size, sum
			counts.Files++
			counts.Bytes += size
		}
		rows = append(rows, row)
		counts.Entries++
		return nil
	})
	return rows, counts, errors.Join(err, root.Close())
}

func hashFile(root *os.Root, name string) (int64, string, error) {
	file, err := root.Open(name)
	if err != nil {
		return 0, "", err
	}
	digest := sha256.New()
	size, err := io.Copy(digest, file)
	if err = errors.Join(err, file.Close()); err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(digest.Sum(nil)), nil
}

func index(rows []Entry) (map[string]Entry, error) {
	if len(rows) == 0 {
		return nil, errors.New("empty manifest")
	}
	result := make(map[string]Entry, len(rows))
	for _, row := range rows {
		if _, exists := result[row.Path]; exists {
			return nil, fmt.Errorf("duplicate manifest path %q", row.Path)
		}
		result[row.Path] = row
	}
	return result, nil
}

// Compare returns the sorted paths whose type, size or hash differ, or that
// only one manifest has.
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
