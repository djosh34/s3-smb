// SPDX-License-Identifier: AGPL-3.0-only

package chaos

import (
	"errors"
	"io/fs"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func reader(files map[string]string) ReadFunc {
	return func(name string) ([]byte, error) {
		data, ok := files[name]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return []byte(data), nil
	}
}

func TestAcknowledged(t *testing.T) {
	l := NewLedger()
	data := []byte("abc")
	l.Write("band", 2, data)
	data[0] = 'X'
	l.Truncate("band", 7)
	l.Write("band", 1, []byte("z"))
	l.Write("gone", 0, []byte("old"))
	l.Remove("gone")
	if err := l.CheckAcknowledged(reader(map[string]string{"band": "\x00zabc\x00\x00"})); err != nil {
		t.Fatal(err)
	}
	for _, files := range []map[string]string{
		{},
		{"band": "\x00zabc\x00"},
		{"band": "\x00zXbc\x00\x00"},
		{"band": "\x00zabc\x00\x00", "gone": "old"},
	} {
		if err := l.CheckAcknowledged(reader(files)); err == nil {
			t.Fatalf("accepted bad files: %q", files)
		}
	}
	if names := l.Names(); strings.Join(names, ",") != "band,gone" {
		t.Fatal(names)
	}
}

func TestTruncateDiscardsTail(t *testing.T) {
	l := NewLedger()
	l.Write("file", 0, []byte("abcdef"))
	l.Truncate("file", 2)
	l.Truncate("file", 6)
	if err := l.CheckAcknowledged(reader(map[string]string{"file": "ab\x00\x00\x00\x00"})); err != nil {
		t.Fatal(err)
	}
	if err := l.CheckAcknowledged(reader(map[string]string{"file": "abcdef"})); err == nil {
		t.Fatal("accepted old tail")
	}
	l.Remove("file")
	l.Write("file", 2, []byte("x"))
	if err := l.CheckAcknowledged(reader(map[string]string{"file": "\x00\x00x"})); err != nil {
		t.Fatal(err)
	}
}

func TestFlushedByteChoices(t *testing.T) {
	l := NewLedger()
	l.Write("file", 0, []byte("abcd"))
	l.Flush("file")
	l.Write("file", 1, []byte("XY"))
	l.Write("file", 2, []byte("Z"))
	for _, data := range []string{"abcd", "aXYd", "aXZd", "aXcd", "abZd"} {
		if err := l.CheckFlushed(reader(map[string]string{"file": data})); err != nil {
			t.Fatalf("%q: %v", data, err)
		}
	}
	for _, files := range []map[string]string{{}, {"file": "aZZd"}, {"file": "aXZe"}, {"file": "aXZ"}, {"file": "aXZde"}} {
		if err := l.CheckFlushed(reader(files)); err == nil {
			t.Fatalf("accepted bad files %q", files)
		}
	}
	l.Flush("file")
	if err := l.CheckFlushed(reader(map[string]string{"file": "abcd"})); err == nil {
		t.Fatal("second flush failed to protect latest state")
	}
}

func TestFlushedLifecycle(t *testing.T) {
	l := NewLedger()
	l.Write("removed", 0, []byte("old"))
	l.Flush("removed")
	l.Remove("removed")
	l.Flush("removed")
	l.Truncate("empty", 0)
	l.Flush("empty")
	l.Write("new", 0, []byte("new"))
	if err := l.CheckFlushed(reader(map[string]string{"empty": ""})); err != nil {
		t.Fatal(err)
	}
	if err := l.CheckFlushed(reader(map[string]string{"empty": "", "removed": "old"})); err == nil {
		t.Fatal("accepted resurrection of flushed removal")
	}
	if err := l.CheckFlushed(reader(map[string]string{"empty": "x"})); err == nil {
		t.Fatal("accepted extra byte")
	}
}

func TestFlushedGrowthAndSizeChanges(t *testing.T) {
	l := NewLedger()
	l.Write("file", 0, []byte("ab"))
	l.Flush("file")
	l.Write("file", 2, []byte("cd"))
	for _, data := range []string{"ab", "abc", "abcd", "ab\x00d"} {
		if err := l.CheckFlushed(reader(map[string]string{"file": data})); err != nil {
			t.Fatalf("partial growth %q: %v", data, err)
		}
	}
	l.Truncate("file", 1)
	if err := l.CheckFlushed(reader(map[string]string{"file": "a"})); err != nil {
		t.Fatal(err)
	}
	l.Remove("file")
	if err := l.CheckFlushed(reader(nil)); err != nil {
		t.Fatal(err)
	}
	l.Flush("file")
	if err := l.CheckFlushed(reader(map[string]string{"file": "ab"})); err == nil {
		t.Fatal("accepted flushed removal resurrection")
	}
}

func TestSettledAttemptAtFlushAndMark(t *testing.T) {
	l := NewLedger()
	l.Attempt("file", 0, []byte("abc"))
	l.Write("file", 0, []byte("abc"))
	l.Flush("file")
	m := l.Mark()
	l.Attempt("file", 0, []byte("XYZ"))
	if err := l.CheckMark(m, reader(map[string]string{"file": "abc"})); err != nil {
		t.Fatal(err)
	}
	for _, check := range []func(ReadFunc) error{l.CheckFlushed, func(read ReadFunc) error { return l.CheckMark(m, read) }} {
		if err := check(reader(map[string]string{"file": "ab"})); err == nil {
			t.Fatal("settled attempt retained uncertain length")
		}
		if err := check(reader(nil)); err == nil {
			t.Fatal("settled attempt retained uncertain existence")
		}
	}
}

func TestZeroLengthOperations(t *testing.T) {
	l := NewLedger()
	l.Write("untouched", 100, nil)
	l.Attempt("untouched", 100, nil)
	l.Flush("untouched")
	l.Truncate("empty", 0)
	if err := l.CheckAcknowledged(reader(map[string]string{"empty": ""})); err != nil {
		t.Fatal(err)
	}
	if err := l.CheckAcknowledged(reader(map[string]string{"empty": "", "untouched": ""})); err == nil {
		t.Fatal("zero-length write created a file")
	}
}

func TestMark(t *testing.T) {
	l := NewLedger()
	l.Write("file", 0, []byte("backup"))
	l.Remove("gone")
	m := l.Mark()
	l.Write("file", 0, []byte("latest"))
	l.Attempt("file", 0, []byte("failed"))
	l.Write("later", 0, []byte("new"))
	l.Write("gone", 0, []byte("back"))
	if err := l.CheckMark(m, reader(map[string]string{"file": "backup"})); err != nil {
		t.Fatal(err)
	}
	for _, files := range []map[string]string{
		{},
		{"file": "latest"},
		{"file": "failed"},
		{"file": "backup", "later": "new"},
		{"file": "backup", "gone": "back"},
		{"file": "backup!"},
	} {
		if err := l.CheckMark(m, reader(files)); err == nil {
			t.Fatalf("accepted bad mark files: %q", files)
		}
	}
}

func TestAttempt(t *testing.T) {
	l := NewLedger()
	l.Write("file", 0, []byte("ab"))
	l.Flush("file")
	data := []byte("XYZ")
	l.Attempt("file", 1, data)
	data[0] = '!'
	m := l.Mark()
	checks := []func(ReadFunc) error{l.CheckAcknowledged, l.CheckFlushed, func(read ReadFunc) error { return l.CheckMark(m, read) }}
	for _, check := range checks {
		for _, data := range []string{"ab", "aX", "aXY", "abY", "aXYZ", "ab\x00Z"} {
			if err := check(reader(map[string]string{"file": data})); err != nil {
				t.Fatalf("%q: %v", data, err)
			}
		}
		for _, files := range []map[string]string{{}, {"file": "a"}, {"file": "a!"}, {"file": "abZZ"}, {"file": "aXYZ!"}} {
			if err := check(reader(files)); err == nil {
				t.Fatalf("accepted bad attempt: %q", files)
			}
		}
	}
	l.Write("file", 1, []byte("XYZ"))
	if err := l.CheckAcknowledged(reader(map[string]string{"file": "ab"})); err == nil {
		t.Fatal("Write failed to settle Attempt")
	}
	if err := l.CheckAcknowledged(reader(map[string]string{"file": "aXYZ"})); err != nil {
		t.Fatal(err)
	}
}

func TestAttemptNewFileAndPartialAcknowledgement(t *testing.T) {
	l := NewLedger()
	l.Attempt("file", 2, []byte("abc"))
	for _, files := range []map[string]string{{}, {"file": ""}, {"file": "\x00\x00a"}, {"file": "\x00\x00a\x00c"}} {
		if err := l.CheckAcknowledged(reader(files)); err != nil {
			t.Fatal(err)
		}
	}
	l.Write("file", 2, []byte("a"))
	if err := l.CheckAcknowledged(reader(map[string]string{"file": "\x00\x00abc"})); err != nil {
		t.Fatal(err)
	}
	if err := l.CheckAcknowledged(reader(map[string]string{"file": "\x00\x00\x00bc"})); err == nil {
		t.Fatal("lost acknowledged prefix")
	}
}

func TestLedgerErrors(t *testing.T) {
	failure := errors.New("read failed")
	l := NewLedger()
	l.Truncate("empty", 0)
	if err := l.CheckAcknowledged(func(string) ([]byte, error) { return nil, failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := l.CheckAcknowledged(nil); err == nil {
		t.Fatal("accepted nil reader")
	}
	for _, op := range []func(*Ledger){
		func(l *Ledger) { l.Write("f", -1, []byte("a")) },
		func(l *Ledger) { l.Attempt("f", math.MaxInt64, []byte("a")) },
		func(l *Ledger) { l.Truncate("f", -1) },
	} {
		l := NewLedger()
		op(l)
		m := l.Mark()
		for _, check := range []func(ReadFunc) error{l.CheckAcknowledged, l.CheckFlushed, func(read ReadFunc) error { return l.CheckMark(m, read) }} {
			if err := check(reader(nil)); err == nil {
				t.Fatal("accepted invalid range")
			}
		}
	}
}

func TestLedgerConcurrentSnapshots(t *testing.T) {
	var l Ledger
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			name := strconv.Itoa(i)
			for range 20 {
				l.Attempt(name, 0, []byte("x"))
				l.Write(name, 0, []byte("x"))
				l.Flush(name)
				l.Names()
				l.Mark()
			}
		})
	}
	wg.Wait()
	if len(l.Names()) != 8 {
		t.Fatal(l.Names())
	}
}
