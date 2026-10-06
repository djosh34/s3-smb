//go:build macos

// SPDX-License-Identifier: AGPL-3.0-only

package macos

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/djosh34/s3-smb/test/macos/helpers"
)

// thinning backs up a file that only the oldest backup holds and deletes
// that backup with tmutil. APFS frees its blocks while the image is mounted.
// The next backup and compacting the image, as Time Machine does to reclaim
// space, give the freed bands back to the share. Then every chunk of a band
// that the file filled must go to the trash and be deleted. A band that also
// holds other data stays, as SMB cannot punch holes. s3-smb keeps 2 copies
// and makes one every 2 minutes here. The remaining backups must restore.
func (h *harness) thinning() result {
	const size = 2 << 30
	h.prepare = func() { h.markedFile("oldest-only.bin", size) }
	outcome := h.baseline()
	outcome.Scenario = "thinning"
	h.must(h.proofDir.Remove("oldest-only.bin"))
	second, secondTree := h.incremental("second", outcome.Baseline, func() {
		h.must(h.proofDir.WriteFile("nested/message.txt", []byte("changed in the second backup\n"), 0o600))
	})
	third, thirdTree := h.incremental("third", second, func() {
		h.randomFile("third.bin", 64<<20)
	})
	holders := h.markedChunks()
	if len(holders)*(8<<20) < size/2 {
		h.t.Fatalf("only %d chunks of whole bands hold the oldest backup's file", len(holders))
	}
	// Freed chunks go through the trash before they are deleted. Copies are
	// kept only minutes here, so the watch starts before anything is freed.
	trashed := h.watchTrash(holders)
	h.deleteBackup(outcome.Baseline, size/2)
	fourth, fourthTree := h.incremental("fourth", third, func() {
		h.must(h.proofDir.WriteFile("nested/message.txt", []byte("changed in the fourth backup\n"), 0o600))
	})
	h.compact()
	h.storage("after-compact")
	h.must(h.waitFor("the deleted backup's chunks leaving the files", 10*time.Minute, 10*time.Second, func() (bool, error) {
		count, err := h.holderStates(holders)
		if err == nil && count.leaked > 0 {
			err = fmt.Errorf("%d chunks of the deleted backup are neither in a file, in the trash nor deleted", count.leaked)
		}
		return count.live == 0, err
	}))
	h.must(h.waitFor("the deleted backup's chunks being deleted", 15*time.Minute, 15*time.Second, func() (bool, error) {
		count, err := h.holderStates(holders)
		return count.live+count.trashed+count.leaked == 0, err
	}))
	if !trashed() {
		h.t.Fatal("no chunk of the deleted backup was ever seen in the trash")
	}
	h.storage("after-thinning")
	outcome.Resumed = fourth
	h.restoreBackup(second, secondTree, "restore-second")
	h.restoreBackup(third, thirdTree, "restore-third")
	outcome.ResumedRestore = h.restoreBackup(fourth, fourthTree, "restore-fourth")
	return outcome
}

// markedFile writes a random file of size bytes in which every 4 KiB block
// starts with helpers.Marker.
func (h *harness) markedFile(name string, size int64) {
	file, err := h.proofDir.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	h.must(err)
	writer := bufio.NewWriterSize(file, 1<<20)
	block := make([]byte, 4096)
	copy(block, helpers.Marker)
	for written := int64(0); written < size && err == nil; written += int64(len(block)) {
		if _, err = io.ReadFull(rand.Reader, block[len(helpers.Marker):]); err == nil {
			_, err = writer.Write(block)
		}
	}
	h.must(errors.Join(err, writer.Flush(), file.Close()))
}

// markedChunks returns the live chunk objects of the band files in which
// every chunk holds a block of the marked file. It reads every live chunk.
func (h *harness) markedChunks() map[string]bool {
	tables, err := h.chunkTables()
	h.must(err)
	marked := map[int64]bool{}
	var found int
	for key := range tables.live {
		ctx, cancel := context.WithTimeout(h.ctx, 5*time.Minute)
		object, err := h.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(h.bucket), Key: aws.String(key)})
		if err != nil {
			cancel()
			h.t.Fatal(err)
		}
		data, err := io.ReadAll(object.Body)
		err = errors.Join(err, object.Body.Close())
		cancel()
		h.must(err)
		file := tables.file[key]
		if helpers.HoldsMarker(data) {
			found++
			if _, seen := marked[file]; !seen {
				marked[file] = true
			}
		} else {
			marked[file] = false
		}
	}
	holders := map[string]bool{}
	for key, file := range tables.file {
		if marked[file] {
			holders[key] = true
		}
	}
	h.t.Log("marked-chunks", found, "whole-band", len(holders), "of", len(tables.live))
	return holders
}

// watchTrash looks at the trash every 5 seconds until the returned function
// is called, which reports whether any holder was ever seen there.
func (h *harness) watchTrash(holders map[string]bool) func() bool {
	var seen atomic.Bool
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			tables, err := h.chunkTables()
			if err != nil {
				h.t.Log("trash-watch", err)
			}
			for key := range holders {
				if tables.trash[key] {
					seen.Store(true)
				}
			}
			select {
			case <-stop:
				return
			case <-h.ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
	return func() bool {
		close(stop)
		<-done
		return seen.Load()
	}
}

type holderCount struct {
	live, trashed, deleted, leaked int
}

// holderStates counts the holders that a file still uses, that are in the
// trash, that are deleted, and that are none of these. The tables are read
// before the bucket is listed, and s3-smb deletes a trashed chunk before it
// drops its trash row, so a chunk listed after it left the trash is a leak.
func (h *harness) holderStates(holders map[string]bool) (holderCount, error) {
	var count holderCount
	tables, err := h.chunkTables()
	if err != nil {
		return count, err
	}
	objects := h.objects("chunks/")
	for key := range holders {
		_, stored := objects[key]
		switch {
		case tables.live[key]:
			count.live++
		case tables.trash[key]:
			count.trashed++
		case stored:
			count.leaked++
		default:
			count.deleted++
		}
	}
	h.t.Logf("holders live=%d trashed=%d deleted=%d leaked=%d", count.live, count.trashed, count.deleted, count.leaked)
	return count, nil
}

// deleteBackup deletes one backup with tmutil on the destination image
// attached read-write. APFS frees the backup's blocks in the background while
// the volume is mounted, so the image stays attached until its used space
// has dropped by at least freed bytes.
func (h *harness) deleteBackup(backup string, freed uint64) {
	h.mount()
	bundle := h.bundle()
	devices, volumes := h.attach(bundle, false)
	if len(devices) > 0 {
		h.attachments = append(h.attachments, devices[0])
	}
	if len(devices) == 0 || len(volumes) != 1 {
		h.t.Fatal("unknown Time Machine image volume layout", devices, volumes)
	}
	before := h.used(volumes[0])
	h.run(30*time.Minute, "/usr/bin/tmutil", "delete", "-d", volumes[0], "-t", strings.TrimSuffix(backup, ".backup"))
	if listed := h.run(10*time.Minute, "/usr/bin/tmutil", "listbackups", "-d", volumes[0], "-m"); strings.Contains(listed, backup) {
		h.t.Fatal("tmutil delete left the backup", backup)
	}
	h.must(h.waitFor("APFS freeing the deleted backup", 15*time.Minute, 10*time.Second, func() (bool, error) {
		used := h.used(volumes[0])
		h.t.Log("backup-volume-used", used, "before", before)
		return used+freed <= before, nil
	}))
	h.must(h.detach())
	h.t.Log("backup-deleted", backup)
}

// compact gives the image's free bands back to the share.
func (h *harness) compact() {
	h.mount()
	h.run(30*time.Minute, "/usr/bin/hdiutil", "compact", h.bundle())
	h.must(h.detach())
}

// bundle returns the one Time Machine image on the mounted share.
func (h *harness) bundle() string {
	bundles, err := filepath.Glob(filepath.Join(h.share, "*.sparsebundle"))
	h.must(err)
	if len(bundles) != 1 {
		h.t.Fatal("expected one real Time Machine sparsebundle", bundles)
	}
	return bundles[0]
}

// used returns the bytes in use on a mounted volume.
func (h *harness) used(volume string) uint64 {
	var stat syscall.Statfs_t
	h.must(syscall.Statfs(volume, &stat))
	return (stat.Blocks - stat.Bfree) * uint64(stat.Bsize)
}
