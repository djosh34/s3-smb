package state_test

import (
	"encoding/binary"
	"fmt"
	"sync"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func TestConcurrentOpenTransitions(t *testing.T) {
	table := newTable(t)
	var workers sync.WaitGroup
	workers.Add(12)
	failures := make(chan error, 12)
	for worker := uint64(0); worker < 12; worker++ {
		go func() {
			defer workers.Done()
			if err := exerciseOpens(table, worker); err != nil {
				failures <- err
			}
		}()
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if actions := table.CloseAll(); len(actions) != 0 {
		t.Fatalf("concurrent transitions leaked %d opens", len(actions))
	}
}

func exerciseOpens(table *state.Table, worker uint64) error {
	for iteration := uint64(0); iteration < 20; iteration++ {
		req := request(1)
		req.Binding.SessionID = worker + 1
		binary.LittleEndian.PutUint64(req.CreateGUID[:8], worker+1)
		binary.LittleEndian.PutUint64(req.CreateGUID[8:], iteration+1)
		token, status := table.Reserve(req)
		if status != smb.StatusSuccess {
			return fmt.Errorf("reserve: %#x", status)
		}
		if iteration%2 == 0 {
			if status = table.Abort(token); status != smb.StatusSuccess {
				return fmt.Errorf("abort: %#x", status)
			}
			continue
		}
		open, status := table.Commit(token, state.Grant{Handle: &handle{key: req.Object}})
		if status != smb.StatusSuccess {
			return fmt.Errorf("commit: %#x", status)
		}
		if _, status = table.Find(open.ID, open.Binding); status != smb.StatusSuccess {
			return fmt.Errorf("find: %#x", status)
		}
		offset := worker*10000 + iteration*100
		if status = table.Lock(open.ID, open.Binding, []state.Range{{Offset: offset, Length: 10, Exclusive: true}}, false); status != smb.StatusSuccess {
			return fmt.Errorf("lock: %#x", status)
		}
		if status = table.CheckIO(open.ID, open.Binding, offset, 10, true); status != smb.StatusSuccess {
			return fmt.Errorf("I/O: %#x", status)
		}
		if status = table.SetDirectory(open.ID, open.Binding, state.DirectoryCursor{Pattern: "*.band"}); status != smb.StatusSuccess {
			return fmt.Errorf("directory: %#x", status)
		}
		if action, closeStatus := table.Close(open.ID, open.Binding); closeStatus != smb.StatusSuccess || action.Handle != open.Handle {
			return fmt.Errorf("close: %#x", closeStatus)
		}
	}
	return nil
}

func TestReconnectWinsOnceUnderConcurrentCalls(t *testing.T) {
	table := newTable(t)
	req := durableRequest(1, 2)
	open := commit(t, table, req, durableGrant(req))
	table.Disconnect(1)
	var workers sync.WaitGroup
	workers.Add(12)
	statuses := make(chan smb.Status, 12)
	for range 12 {
		go func() {
			defer workers.Done()
			_, status := table.Reconnect(reconnectRequest(open))
			statuses <- status
		}()
	}
	workers.Wait()
	close(statuses)
	var successes int
	for status := range statuses {
		if status == smb.StatusSuccess {
			successes++
		} else {
			statusIs(t, status, smb.StatusObjectNameNotFound)
		}
	}
	if successes != 1 {
		t.Fatalf("reconnect succeeded %d times", successes)
	}
	if len(table.CloseAll()) != 1 {
		t.Fatal("reconnect duplicated or lost the open")
	}
}
