// SPDX-License-Identifier: AGPL-3.0-only

package chaos

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"slices"
	"sync"
)

// ReadFunc reads a file, returning fs.ErrNotExist when it is missing.
// Checks visit only paths recorded in the ledger. Scenarios must check directory
// listings themselves if they need to detect other, unexpected paths.
type ReadFunc func(name string) ([]byte, error)

// Ledger records successful client operations and writes whose outcome is not
// known. Methods are safe for concurrent use, but callers must record operations
// in the order the server applied them. Invalid ranges make all checks fail.
type Ledger struct {
	files map[string]fileHistory
	err   error
	mu    sync.Mutex
}

type fileHistory struct {
	ops     []operation
	flushed int
}

type operation struct {
	data   []byte
	offset int64
	size   int64
	kind   operationKind
}

type operationKind uint8

const (
	writeOp operationKind = iota
	attemptOp
	truncateOp
	removeOp
)

// Mark is an immutable snapshot of a ledger at a verified metadata backup.
type Mark struct {
	files map[string]fileHistory
	err   error
}

// NewLedger returns an empty ledger. Its zero value is also usable.
func NewLedger() *Ledger { return &Ledger{files: make(map[string]fileHistory)} }

// Write records only the bytes the client was told succeeded. It settles an
// earlier Attempt for the same range. The ledger copies data.
func (l *Ledger) Write(name string, offset int64, data []byte) {
	l.record(name, operation{kind: writeOp, offset: offset, data: bytes.Clone(data)})
}

// Attempt records a write before it is sent. Until Write settles the range,
// each byte may keep its earlier value or take the attempted value. The file
// may end anywhere between its earlier length and the end of the attempt.
func (l *Ledger) Attempt(name string, offset int64, data []byte) {
	l.record(name, operation{kind: attemptOp, offset: offset, data: bytes.Clone(data)})
}

// Truncate records a successful size change, including zero-filled growth.
func (l *Ledger) Truncate(name string, size int64) {
	l.record(name, operation{kind: truncateOp, size: size})
}

// Remove records a successful removal. The path remains in Names.
func (l *Ledger) Remove(name string) { l.record(name, operation{kind: removeOp}) }

func (l *Ledger) record(name string, op operation) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.files == nil {
		l.files = make(map[string]fileHistory)
	}
	if op.offset < 0 || op.size < 0 || int64(len(op.data)) > math.MaxInt64-op.offset {
		l.err = errors.Join(l.err, fmt.Errorf("%q: invalid ledger range", name))
		return
	}
	file := l.files[name]
	file.ops = append(file.ops, op)
	l.files[name] = file
}

// Flush records a successful flush of the current file state.
func (l *Ledger) Flush(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.files == nil {
		l.files = make(map[string]fileHistory)
	}
	file := l.files[name]
	file.flushed = len(file.ops)
	l.files[name] = file
}

// Names returns every touched path, including removed paths, in sorted order.
func (l *Ledger) Names() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return sortedNames(l.files)
}

func sortedNames(files map[string]fileHistory) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Mark captures the state a verified metadata backup protects. Call it with no
// write in flight between the backup snapshot and this call, or record those
// writes as attempts. Later operations do not change the mark.
func (l *Ledger) Mark() Mark {
	l.mu.Lock()
	defer l.mu.Unlock()
	files := make(map[string]fileHistory, len(l.files))
	for name, file := range l.files {
		file.ops = slices.Clone(file.ops)
		files[name] = file
	}
	return Mark{files: files, err: l.err}
}

// CheckAcknowledged checks that every acknowledged change survived a drop.
func (l *Ledger) CheckAcknowledged(read ReadFunc) error {
	return checkFiles(l.Mark(), read, false)
}

// CheckFlushed checks a crash with local disk intact. Flushed changes must
// survive. Each later changed byte may have its flushed value or a value from
// a later acknowledged write. Unflushed growth may end partway through a write.
// Pending attempts have the same byte allowance.
func (l *Ledger) CheckFlushed(read ReadFunc) error {
	return checkFiles(l.Mark(), read, true)
}

// CheckMark checks recovery against the captured backup. It also checks that
// paths first recorded after the mark are absent. Only attempts pending at the
// mark are allowed, not attempts recorded later.
func (l *Ledger) CheckMark(m Mark, read ReadFunc) error {
	current := l.Mark()
	if current.err != nil {
		return current.err
	}
	files := make(map[string]fileHistory, len(current.files)+len(m.files))
	for name := range current.files {
		files[name] = fileHistory{}
	}
	for name, file := range m.files {
		files[name] = file
	}
	return checkFiles(Mark{files: files, err: m.err}, read, false)
}

func checkFiles(mark Mark, read ReadFunc, crash bool) error {
	if mark.err != nil {
		return mark.err
	}
	if read == nil {
		return errors.New("nil ledger reader")
	}
	for _, name := range sortedNames(mark.files) {
		file := mark.files[name]
		allowed := []possibleFile{{missing: true}}
		for i, op := range file.ops {
			optional := op.kind == attemptOp || (crash && i >= file.flushed)
			allowed = applyOperation(allowed, op, optional)
		}
		data, readErr := read(name)
		var failure error
		for _, state := range allowed {
			failure = state.check(name, data, readErr)
			if failure == nil {
				break
			}
		}
		if failure != nil {
			return failure
		}
	}
	return nil
}

// A layer either fixes a range or adds an allowed value to it. Keeping ranges,
// rather than a set per byte, makes the ledger small for large band writes.
type byteLayer struct {
	data     []byte
	start    int64
	end      int64
	required bool
}

type sizeRange struct{ low, high int64 }

type possibleFile struct {
	layers  []byteLayer
	sizes   []sizeRange
	missing bool
}

// Optional size and existence changes fork the structural outcome. Their zero
// fill belongs only to that outcome, never to a file that kept its old length.
// Writes still combine byte choices within each outcome, as rule 2 requires.
func applyOperation(states []possibleFile, op operation, optional bool) []possibleFile {
	if optional && (op.kind == truncateOp || op.kind == removeOp) {
		count := len(states)
		for i := range count {
			changed := states[i]
			changed.layers = slices.Clone(changed.layers)
			changed.sizes = slices.Clone(changed.sizes)
			changed.apply(op, false)
			states = append(states, changed)
		}
		return states
	}
	for i := range states {
		states[i].apply(op, optional)
	}
	return states
}

func (p *possibleFile) apply(op operation, optional bool) {
	switch op.kind {
	case writeOp, attemptOp:
		p.write(op, optional)
	case truncateOp:
		p.truncate(op.size)
	case removeOp:
		p.layers = append(p.layers, byteLayer{end: math.MaxInt64, required: true})
		p.missing = true
		p.sizes = nil
	}
}

func (p *possibleFile) write(op operation, optional bool) {
	if len(op.data) == 0 {
		return
	}
	end := op.offset + int64(len(op.data))
	grown := make([]sizeRange, 0, len(p.sizes)+1)
	for _, size := range p.sizes {
		if optional {
			grown = append(grown, sizeRange{low: size.low, high: max(size.high, end)})
		} else {
			grown = append(grown, sizeRange{low: max(size.low, end), high: max(size.high, end)})
		}
	}
	if p.missing {
		low := end
		if optional {
			low = 0
		}
		grown = append(grown, sizeRange{low: low, high: end})
	}
	p.setSizes(grown, optional)
	p.layers = append(p.layers, byteLayer{start: op.offset, end: end, data: op.data, required: !optional})
}

func (p *possibleFile) truncate(size int64) {
	// Bytes past the new end must be zero if a later operation grows the file.
	p.layers = append(p.layers, byteLayer{start: size, end: math.MaxInt64, required: true})
	p.setSizes([]sizeRange{{low: size, high: size}}, false)
}

func (p *possibleFile) setSizes(sizes []sizeRange, optional bool) {
	if optional {
		p.sizes = append(p.sizes, sizes...)
	} else {
		p.sizes = sizes
		p.missing = false
	}
	slices.SortFunc(p.sizes, func(a, b sizeRange) int {
		if a.low < b.low {
			return -1
		}
		if a.low > b.low {
			return 1
		}
		return 0
	})
	merged := p.sizes[:0]
	for _, size := range p.sizes {
		if len(merged) != 0 && size.low <= merged[len(merged)-1].high {
			merged[len(merged)-1].high = max(merged[len(merged)-1].high, size.high)
		} else {
			merged = append(merged, size)
		}
	}
	p.sizes = merged
}

func (p *possibleFile) check(name string, data []byte, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		if p.missing {
			return nil
		}
		return fmt.Errorf("%q: missing file", name)
	}
	if err != nil {
		return fmt.Errorf("read %q: %w", name, err)
	}
	if !slices.ContainsFunc(p.sizes, func(size sizeRange) bool {
		return int64(len(data)) >= size.low && int64(len(data)) <= size.high
	}) {
		return fmt.Errorf("%q: unexpected length %d", name, len(data))
	}
	for offset, value := range data {
		if !p.acceptsByte(int64(offset), value) {
			return fmt.Errorf("%q: unexpected byte at offset %d: 0x%02x", name, offset, value)
		}
	}
	return nil
}

func (p *possibleFile) acceptsByte(offset int64, value byte) bool {
	for i := len(p.layers) - 1; i >= 0; i-- {
		layer := p.layers[i]
		if offset < layer.start || offset >= layer.end {
			continue
		}
		var candidate byte
		if layer.data != nil {
			candidate = layer.data[offset-layer.start]
		}
		if value == candidate {
			return true
		}
		if layer.required {
			return false
		}
	}
	return value == 0
}
