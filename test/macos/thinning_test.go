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
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/djosh34/s3-smb/test/macos/helpers"
)

// thinning backs up a file that only the oldest backup holds, deletes that
// backup with tmutil and checks that the chunks holding the file go to the
// trash and are then deleted. Only band files that the file fills count: a
// band that also holds other data stays, and s3-smb cannot punch holes. s3-smb
// keeps 2 copies and makes one every 2 minutes here. The remaining backups
// must restore.
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
	freed := func() (bool, error) {
		count, err := h.holderStates(holders)
		if err == nil && count.leaked > 0 {
			err = fmt.Errorf("%d chunks of the deleted backup are neither in a file, in the trash nor deleted", count.leaked)
		}
		return count.live == 0, err
	}
	probeStart := time.Now().UTC()
	h.deleteBackup(outcome.Baseline, freed)
	h.storage("after-delete")
	h.reclaimProbe(probeStart, freed)
	h.must(h.waitFor("the deleted backup's chunks leaving the files", 10*time.Minute, 10*time.Second, freed))
	h.must(h.waitFor("the deleted backup's chunks being deleted", 15*time.Minute, 15*time.Second, func() (bool, error) {
		count, err := h.holderStates(holders)
		return count.live+count.trashed+count.leaked == 0, err
	}))
	h.storage("after-thinning")
	outcome.Resumed = third
	h.restoreBackup(second, secondTree, "restore-second")
	outcome.ResumedRestore = h.restoreBackup(third, thirdTree, "restore-third")
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

// deleteBackup deletes one backup with tmutil, as thinning does, on the
// destination image attached read-write. APFS may free the blocks in the
// background, so the image stays attached for up to 5 minutes until freed
// reports true.
func (h *harness) deleteBackup(backup string, freed func() (bool, error)) {
	h.mount()
	bundles, err := filepath.Glob(filepath.Join(h.share, "*.sparsebundle"))
	h.must(err)
	if len(bundles) != 1 {
		h.t.Fatal("expected one real Time Machine sparsebundle", bundles)
	}
	devices, volumes := h.attach(bundles[0], false)
	if len(devices) == 0 || len(volumes) != 1 {
		h.t.Fatal("unknown Time Machine image volume layout", devices, volumes)
	}
	h.attachments = append(h.attachments, devices[0])
	h.run(30*time.Minute, "/usr/bin/tmutil", "delete", "-d", volumes[0], "-t", strings.TrimSuffix(backup, ".backup"))
	if listed := h.run(10*time.Minute, "/usr/bin/tmutil", "listbackups", "-d", volumes[0], "-m"); strings.Contains(listed, backup) {
		h.t.Fatal("tmutil delete left the backup", backup)
	}
	err = h.waitFor("chunks freed while the image is attached", 5*time.Minute, 10*time.Second, freed)
	if err != nil && (h.ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded)) {
		h.must(err)
	}
	h.must(h.detach())
	h.t.Log("backup-deleted", backup)
}

// reclaimProbe is temporary: it tries what may make macOS give the deleted
// backup's bands back, and logs the holders after each step.
func (h *harness) reclaimProbe(start time.Time, freed func() (bool, error)) {
	step := func(label string) {
		done, err := freed()
		h.t.Log("reclaim-probe", label, done, err)
	}
	bands := func() {
		bundles, err := filepath.Glob(filepath.Join(h.share, "*.sparsebundle"))
		h.must(err)
		h.run(2*time.Minute, "/bin/ls", "-l", filepath.Join(bundles[0], "bands"))
		h.run(2*time.Minute, "/usr/bin/du", "-sk", bundles[0])
	}
	step("detached")
	h.mount()
	bands()
	bundles, err := filepath.Glob(filepath.Join(h.share, "*.sparsebundle"))
	h.must(err)
	devices, _ := h.attach(bundles[0], false)
	if len(devices) > 0 {
		h.attachments = append(h.attachments, devices[0])
	}
	err = h.waitFor("chunks freed after a new attach", 3*time.Minute, 15*time.Second, freed)
	h.t.Log("reclaim-probe reattached", err)
	bands()
	h.must(h.detach())
	step("reattached and detached")
	h.mount()
	output, err := h.try(30*time.Minute, "/usr/bin/hdiutil", "compact", bundles[0])
	h.t.Log("reclaim-probe compact", output, err)
	bands()
	h.must(h.detach())
	step("compacted")
	format := "2006-01-02 15:04:05-0700"
	_, err = h.try(10*time.Minute, "/usr/bin/log", "show", "--style", "json", "--start", start.Format(format), "--info", "--debug", "--predicate", `process == "diskimagesiod" OR process == "diskimages-helper" OR subsystem BEGINSWITH "com.apple.DiskImages" OR senderImagePath CONTAINS "smbfs" OR process == "apfsd" OR senderImagePath CONTAINS "apfs"`)
	h.t.Log("reclaim-probe log", err)
}
