// SPDX-License-Identifier: AGPL-3.0-only
package smbfs

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

func TestConcurrentNativeHandles(t *testing.T) {
	s := newFixture(t).s
	const count = 8
	var wg sync.WaitGroup
	failures := make(chan error, count)
	for i := 0; i < count; i++ {
		h := openFile(t, s, fmt.Sprintf("file-%d", i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			data := bytes.Repeat([]byte{byte(h)}, 4096)
			if _, e := s.Write(h, data, 0, 0); e != nil {
				failures <- e
				return
			}
			if e := s.Flush(h); e != nil {
				failures <- e
				return
			}
			got := make([]byte, len(data))
			n, e := s.Read(h, got, 0, 0)
			if e != nil {
				failures <- e
				return
			}
			if !bytes.Equal(got[:n], data) {
				failures <- fmt.Errorf("handle %d content mismatch", h)
				return
			}
			if e = s.Close(h); e != nil {
				failures <- e
			}
		}()
	}
	wg.Wait()
	close(failures)
	for e := range failures {
		t.Error(e)
	}
}
