// SPDX-License-Identifier: AGPL-3.0-only

package helpers

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// Range is a run of 4 KiB blocks that differ between two files. Zero tells
// whether the actual file holds only zeros there.
type Range struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	Zero  bool  `json:"zero"`
}

// DiffBlocks compares two files in 4 KiB blocks and returns up to 1000
// ranges where they differ, merging neighbours that are both zero or both
// not.
func DiffBlocks(expected, actual string) (ranges []Range, err error) {
	left, err := os.Open(expected) //nolint:gosec // The harness names its own files.
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, left.Close()) }()
	right, err := os.Open(actual) //nolint:gosec // The harness names its own files.
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, right.Close()) }()
	a, b, zero := make([]byte, 4096), make([]byte, 4096), make([]byte, 4096)
	for offset := int64(0); len(ranges) < 1000; offset += 4096 {
		n, errA := io.ReadFull(left, a)
		m, errB := io.ReadFull(right, b)
		if n != m {
			return append(ranges, Range{Start: offset, End: -1}), nil
		}
		if n == 0 {
			return ranges, readEnd(errA, errB)
		}
		if !bytes.Equal(a[:n], b[:m]) {
			isZero := bytes.Equal(b[:m], zero[:m])
			if last := len(ranges) - 1; last >= 0 && ranges[last].End == offset && ranges[last].Zero == isZero {
				ranges[last].End = offset + int64(n)
			} else {
				ranges = append(ranges, Range{Start: offset, End: offset + int64(n), Zero: isZero})
			}
		}
		if errA != nil || errB != nil {
			return ranges, readEnd(errA, errB)
		}
	}
	return ranges, nil
}

// readEnd returns the read errors that are not the end of a file.
func readEnd(errs ...error) error {
	var result error
	for _, err := range errs {
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			result = errors.Join(result, err)
		}
	}
	return result
}
