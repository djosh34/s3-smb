package server

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

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
