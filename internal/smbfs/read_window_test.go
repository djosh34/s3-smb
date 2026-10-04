package smbfs

import (
	"errors"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
)

func TestReadRetryWindowOption(t *testing.T) {
	fixture := newFixture(t, 0)
	if fixture.fs.readWindow != readRetryWindow || readRetryWindow != 360*time.Second {
		t.Fatalf("production read retry window = %s", fixture.fs.readWindow)
	}
	options := Options{Filesystem: fixture.native, Barrier: fixture.fs.barrier, Config: fixture.config, Store: fixture.chunks, MetadataPath: fixture.path, ReadRetryWindow: -time.Second}
	if adapter, err := New(options); !errors.Is(err, smb.ErrInvalidParameter) || adapter != nil {
		t.Fatalf("negative retry window: adapter %v, error %v", adapter, err)
	}
	options.ReadRetryWindow = 100 * time.Millisecond
	adapter, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.readWindow != options.ReadRetryWindow {
		t.Errorf("configured read retry window = %s", adapter.readWindow)
	}
	if err := adapter.Shutdown(); err != nil {
		t.Fatal(err)
	}
}
