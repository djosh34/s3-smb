// SPDX-License-Identifier: AGPL-3.0-only
package smbfs

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
)

func TestReportedSpace(t *testing.T) {
	const tib, pib = uint64(1) << 40, uint64(1) << 50
	for _, c := range []struct{ total, avail, wantTotal, wantAvail uint64 }{
		{1 << 30, 1 << 29, 1 << 30, 1 << 29},   // small volume: unchanged
		{pib, pib, tib, tib},                   // empty unlimited volume
		{pib, pib - 19<<30, 19<<30 + tib, tib}, // used space is kept
		{3 * tib, tib / 2, 3 * tib, tib / 2},   // less than the limit left
		{pib, pib - 5*tib, 5*tib + tib, tib},   // more used than the limit
	} {
		total, avail := reportedSpace(c.total, c.avail)
		if total != c.wantTotal || avail != c.wantAvail {
			t.Errorf("reportedSpace(%d, %d) = %d, %d; want %d, %d", c.total, c.avail, total, avail, c.wantTotal, c.wantAvail)
		}
	}
}

// An unlimited volume reports 1 PiB. Time Machine sizes its sparsebundle bands
// from that and formats the image by writing about 19 GB of zeros.
func TestStatFSLimitsUnlimitedVolume(t *testing.T) {
	f := newFixture(t)
	if e := f.m.Init(&meta.Format{Name: "adapter-test", UUID: "adapter-fixture", Storage: "file", BlockSize: 64, Compression: "none", TrashDays: 14, DirStats: true}, true); e != nil {
		t.Fatal(e)
	}
	a, e := f.s.StatFS(0)
	if e != nil {
		t.Fatal(e)
	}
	blocks, _ := a.GetBlocks()
	free, _ := a.GetAvailableBlocks()
	if free != (1<<40)/4096 || blocks < free || blocks > free+(1<<30)/4096 {
		t.Fatalf("blocks=%d available=%d; want 1 TiB available", blocks, free)
	}
}
