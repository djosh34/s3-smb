package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
)

type filesMetaCleanupRecorder struct {
	*testing.T
	cleanups []func()
	reported []string
}

func (test *filesMetaCleanupRecorder) Cleanup(cleanup func()) {
	test.cleanups = append(test.cleanups, cleanup)
}

func (test *filesMetaCleanupRecorder) Error(args ...any) {
	test.reported = append(test.reported, fmt.Sprint(args...))
}

func (test *filesMetaCleanupRecorder) finish() {
	for len(test.cleanups) > 0 {
		last := len(test.cleanups) - 1
		cleanup := test.cleanups[last]
		test.cleanups = test.cleanups[:last]
		cleanup()
	}
}

type filesMetaCloseFailure struct {
	smb.Storage
	err error
}

func (storage *filesMetaCloseFailure) Close(ctx context.Context, handle smb.Handle) error {
	return errors.Join(storage.Storage.Close(ctx, handle), storage.err)
}

func TestFilesMetaClientReportsDisconnectCleanup(t *testing.T) {
	for _, failure := range []error{
		nil, errors.New("injected disconnect close failure"), net.ErrClosed,
		context.Canceled, context.DeadlineExceeded,
		errors.Join(context.Canceled, errors.New("joined disconnect close failure")),
	} {
		name := "normal"
		if failure != nil {
			name = failure.Error()
		}
		t.Run(name, func(t *testing.T) {
			options := testOptions(t)
			options.Storage = &filesMetaCloseFailure{Storage: newFilesMetaStorage(t), err: failure}
			server, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			recorder := &filesMetaCleanupRecorder{T: t}
			t.Cleanup(recorder.finish)
			_, _, session := newFilesMetaClient(recorder, server)
			insertFilesMetaOpen(t, server, session, "disconnect", fileReadData)
			recorder.finish()
			if failure == nil {
				if len(recorder.reported) != 0 {
					t.Fatalf("normal disconnect reported errors: %v", recorder.reported)
				}
			} else if len(recorder.reported) != 1 || !strings.Contains(recorder.reported[0], failure.Error()) {
				t.Fatalf("cleanup failure not reported once: %v", recorder.reported)
			}
		})
	}
}
