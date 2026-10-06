// SPDX-License-Identifier: AGPL-3.0-only
package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

// Throwaway research test for #620: concurrent writers on several files, a
// RAM budget of a few chunks so early uploads cross files, S3 that fails and
// stalls at random, truncates, flushes, closes and reads, all checked against
// a model. A write that fails is retried until it lands, as a client would.
func TestZZOutageStress(t *testing.T) {
	for round := range 30 {
		t.Run(fmt.Sprint(round), func(t *testing.T) { outageRound(t, uint64(round)) })
	}
}

type flakyBucket struct {
	*memBucket
	failRate atomic.Uint32 // out of 100
	stall    atomic.Bool
	rnd      *rand.Rand
	mu       sync.Mutex
}

func (b *flakyBucket) gate(ctx context.Context) error {
	for b.stall.Load() {
		if !sleep(ctx, time.Millisecond) {
			return ctx.Err()
		}
	}
	b.mu.Lock()
	fail := b.rnd.Uint32N(100) < b.failRate.Load()
	b.mu.Unlock()
	if fail {
		injected.Add(1)
		return errors.New("injected S3 failure")
	}
	return nil
}

func (b *flakyBucket) put(ctx context.Context, key string, data []byte) error {
	puts.Add(1)
	if err := b.gate(ctx); err != nil {
		return err
	}
	return b.memBucket.put(ctx, key, data)
}

func (b *flakyBucket) get(ctx context.Context, key string, offset, length uint64) ([]byte, error) {
	if err := b.gate(ctx); err != nil {
		return nil, err
	}
	return b.memBucket.get(ctx, key, offset, length)
}

var writeFails, flushFails, evictions, puts, injected atomic.Int64

func TestZZOutageStressCounts(t *testing.T) {
	t.Cleanup(func() {
		t.Logf("puts %d injected failures %d early uploads %d failed writes %d failed flushes %d",
			puts.Load(), injected.Load(), evictions.Load(), writeFails.Load(), flushFails.Load())
	})
	for round := range 5 {
		t.Run(fmt.Sprint(round), func(t *testing.T) { outageRound(t, uint64(100+round)) })
	}
}

func outageRound(t *testing.T, seed uint64) {
	f := newFixture(t)
	f.tune.chunkSize = 64
	f.tune.dirtyChunks = 4
	f.tune.readChunks = 3
	flaky := &flakyBucket{memBucket: f.bucket, rnd: rand.New(rand.NewPCG(seed, 99))}
	e, err := open(context.Background(), Options{Dir: f.dir}, flaky, f.tune)
	if err != nil {
		t.Fatal(err)
	}
	const files = 6
	const span = 2048
	want := make([][]byte, files)
	for i := range files {
		create(t, e, fmt.Sprintf("f%d", i), smb.KindFile)
	}
	stop := make(chan struct{})
	var chaos sync.WaitGroup
	chaos.Go(func() {
		r := rand.New(rand.NewPCG(seed, 7))
		for {
			select {
			case <-stop:
				flaky.stall.Store(false)
				flaky.failRate.Store(0)
				return
			default:
			}
			flaky.failRate.Store(r.Uint32N(60))
			flaky.stall.Store(r.IntN(3) == 0)
			time.Sleep(time.Duration(r.IntN(3)) * time.Millisecond)
		}
	})
	var writers sync.WaitGroup
	errs := make(chan error, files)
	for i := range files {
		writers.Go(func() {
			errs <- writeLoop(e, i, seed, span, &want[i])
		})
	}
	writers.Wait()
	close(stop)
	chaos.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	check := func(e *Engine, when string) {
		for i := range files {
			got := readFile(t, e, fmt.Sprintf("f%d", i))
			if !bytes.Equal([]byte(got), want[i]) {
				t.Fatalf("%s: file %d differs: %s", when, i, describe(want[i], []byte(got), f.tune.chunkSize))
			}
		}
	}
	check(e, "live")
	if err := e.flushAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	kill(t, e)
	e = f.open()
	check(e, "after a crash following a full flush")
}

func writeLoop(e *Engine, i int, seed uint64, span int, model *[]byte) error {
	ctx := context.Background()
	r := rand.New(rand.NewPCG(seed, uint64(i)))
	path := fmt.Sprintf("f%d", i)
	open := func() (smb.Handle, error) {
		res, err := e.Lookup(ctx, path)
		if err != nil {
			return nil, err
		}
		return e.Open(ctx, res.Object, smb.AccessRead|smb.AccessWrite)
	}
	h, err := open()
	if err != nil {
		return err
	}
	for range 300 {
		switch op := r.IntN(20); {
		case op < 12:
			offset := r.IntN(span)
			data := make([]byte, 1+r.IntN(200))
			for k := range data {
				data[k] = byte(r.IntN(255) + 1)
			}
			for {
				wctx, cancel := context.WithTimeout(ctx, time.Duration(r.IntN(5))*time.Millisecond)
				_, err = e.WriteAt(wctx, h, data, uint64(offset))
				cancel()
				if err == nil {
					break
				}
				writeFails.Add(1)
			}
			if need := offset + len(data); len(*model) < need {
				*model = append(*model, make([]byte, need-len(*model))...)
			}
			copy((*model)[offset:], data)
		case op < 14:
			size := r.IntN(span)
			if err = e.Truncate(ctx, h, uint64(size)); err != nil {
				return fmt.Errorf("truncate: %w", err)
			}
			if size < len(*model) {
				*model = (*model)[:size]
			} else {
				*model = append(*model, make([]byte, size-len(*model))...)
			}
		case op < 16:
			if e.Flush(ctx, h, smb.SyncData) != nil { // may fail during an outage; data stays dirty
				flushFails.Add(1)
			}
		case op < 17:
			_ = e.Flush(ctx, h, smb.SyncFull)
		case op < 18:
			if err = e.Close(ctx, h); err != nil {
				return fmt.Errorf("close: %w", err)
			}
			if h, err = open(); err != nil {
				return err
			}
		default:
			got := make([]byte, len(*model)+10)
			var n int
			for {
				rctx, cancel := context.WithTimeout(ctx, time.Duration(r.IntN(5))*time.Millisecond)
				n, err = e.ReadAt(rctx, h, got, 0)
				cancel()
				if err == nil || errors.Is(err, io.EOF) {
					break
				}
			}
			if !bytes.Equal(got[:n], *model) {
				return fmt.Errorf("file %d read differs mid-run: %s", i, describe(*model, got[:n], 64))
			}
		}
	}
	return e.Close(ctx, h)
}

// describe says where got differs from want, by chunk, and whether the wrong
// bytes are zeros.
func describe(want, got []byte, chunk uint64) string {
	if len(want) != len(got) {
		return fmt.Sprintf("size %d, want %d", len(got), len(want))
	}
	var out []string
	for i := 0; i < len(want); i++ {
		if want[i] == got[i] {
			continue
		}
		j := i
		zero := true
		for j < len(want) && want[j] != got[j] {
			zero = zero && got[j] == 0
			j++
		}
		out = append(out, fmt.Sprintf("[%d,%d) chunk %d zero=%v", i, j, uint64(i)/chunk, zero))
		i = j
	}
	return fmt.Sprint(out)
}
