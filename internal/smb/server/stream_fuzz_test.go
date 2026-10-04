package server

import (
	"testing"

	"github.com/djosh34/s3-smb/internal/smb/smbtest"
)

func FuzzServerStream(f *testing.F) {
	for _, seed := range streamSeeds(f) {
		f.Add(seed.stream)
	}
	storage := smbtest.NewStorage(f)
	root, err := storage.Lookup(f.Context(), "")
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, stream []byte) {
		runFuzzInput(t, storage, root.Attr, stream)
	})
}
