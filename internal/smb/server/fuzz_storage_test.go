package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
)

func TestFuzzStorageRuntimeStaysBounded(t *testing.T) {
	storage := smbtest.NewStorage(t)
	root, err := storage.Lookup(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	stream := streamSeeds(t)[0].stream
	t.Run("warmup", func(t *testing.T) { runFuzzInput(t, storage, root.Attr, stream) })
	before := fuzzRuntimeWorkers(t)
	if before < 6 {
		t.Fatalf("runtime worker diagnostic found only %d workers", before)
	}
	for i := range 12 {
		t.Run(fmt.Sprintf("input_%d", i), func(t *testing.T) { runFuzzInput(t, storage, root.Attr, stream) })
		if after := fuzzRuntimeWorkers(t); after != before {
			t.Fatalf("input %d changed runtime workers: %d -> %d", i, before, after)
		}
	}
}

func TestFuzzStorageResetIsolatesMutations(t *testing.T) {
	storage := smbtest.NewStorage(t)
	root, err := storage.Lookup(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	for input := range 2 {
		t.Run(fmt.Sprintf("input_%d", input), func(t *testing.T) {
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), streamBound)
				defer cancel()
				if err := resetFuzzStorage(ctx, storage, root.Attr); err != nil {
					t.Error(err)
				}
			})
			client := newReadWriteClient(t, storage)
			file := createdFile(t, client.create(t, createRequest("file", fileCreateDisposition)))
			writeCreatedFile(t, client, file.ID, "old bytes")
			stream := createdFile(t, client.create(t, createRequest("file:AFP_AfpInfo", fileCreateDisposition)))
			writeCreatedFile(t, client, stream.ID, "old stream")
			request := createRequest("dir", fileCreateDisposition)
			request.Options = fileDirectoryFile
			createdFile(t, client.create(t, request))
			for i := range fuzzResetPageSize + 1 {
				createdFile(t, client.create(t, createRequest(fmt.Sprintf("dir/file-%d", i), fileCreateDisposition)))
			}
			stamp := time.Unix(1700000000, 0).UTC()
			attributes := uint32(0x12)
			if err := storage.SetAttr(t.Context(), root.Object, smb.AttrChange{
				Created: &stamp, Accessed: &stamp, Modified: &stamp, Changed: &stamp, Attributes: &attributes,
			}); err != nil {
				t.Fatal(err)
			}
			// Leave every open live; server cleanup must close them before reset.
		})
		entries, err := storage.ReadDir(t.Context(), root.Object.Inode, 0, fuzzResetPageSize)
		if err != nil || len(entries) != 0 {
			t.Fatalf("input %d retained entries: %+v, %v", input, entries, err)
		}
		after, err := storage.GetAttr(t.Context(), root.Object)
		if err != nil || after != root.Attr {
			t.Fatalf("input %d retained root attributes: %+v, want %+v, %v", input, after, root.Attr, err)
		}
	}
}
