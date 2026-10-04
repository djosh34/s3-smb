package server

import (
	"errors"
	"math"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

// credits is the connection's sequence window. A multi-credit request removes
// consecutive IDs, and a response extends the window exactly once. Only the
// receive loop changes it; final async responses never allocate credits.
type credits struct {
	available map[uint64]struct{}
	next      uint64
}

func newCredits() credits {
	return credits{available: map[uint64]struct{}{0: {}}, next: 1}
}

func creditCharge(header wire.Header) uint16 {
	if header.CreditCharge == 0 {
		return 1
	}
	return header.CreditCharge
}

// consume checks the entire compound before removing any sequence number.
func (credits *credits) consume(messages []wire.Message) error {
	used := make(map[uint64]struct{})
	for _, message := range messages {
		header := message.Header
		if header.Command == wire.Cancel {
			continue
		}
		charge := uint64(creditCharge(header))
		if header.MessageID > math.MaxUint64-charge {
			return errors.New("message ID range overflows")
		}
		for id := header.MessageID; id < header.MessageID+charge; id++ {
			if _, exists := credits.available[id]; !exists {
				return errors.New("message ID is outside the credit window")
			}
			if _, exists := used[id]; exists {
				return errors.New("compound reuses a credit")
			}
			used[id] = struct{}{}
		}
	}
	for id := range used {
		delete(credits.available, id)
	}
	return nil
}

func (credits *credits) grant(header wire.Header) uint16 {
	// Replenish the consumed charge even when CreditRequest is zero or smaller
	// than that charge. Grow only as requested, toward the connection target.
	wanted := max(creditCharge(header), header.Credit)
	if header.Command == wire.SessionSetup {
		wanted = max(wanted, smb.MinReconnectCredits)
	}
	balance := len(credits.available)
	if balance >= int(smb.TargetCredits) {
		return 0
	}
	room := smb.TargetCredits - uint16(balance)
	wanted = min(wanted, room)
	var granted uint16
	for range wanted {
		if credits.next == math.MaxUint64 {
			break
		}
		credits.available[credits.next] = struct{}{}
		credits.next++
		granted++
	}
	return granted
}
