package server

import (
	"context"
	"testing"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestHandlerRequestContext(t *testing.T) {
	options := testOptions(t)
	server, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	server.handlers[wire.Echo] = func(ctx context.Context, request RequestContext, message wire.Message) (reply, error) {
		if request.Storage != options.Storage || request.Opens != options.State {
			t.Error("handler did not receive shared modules")
		}
		if request.Session != (Session{}) || request.Tree != (Tree{}) {
			t.Error("unauthenticated ECHO has an identity")
		}
		return handleEcho(ctx, request, message)
	}
	client, ctx := pipeClient(t, server)
	exchange(ctx, t, client, negotiateMessage(t, 1))
	exchange(ctx, t, client, echo(t, 1))
}

func TestRequestFileID(t *testing.T) {
	placeholder := wire.FileID{Persistent: ^uint64(0), Volatile: ^uint64(0)}
	id := wire.FileID{Persistent: 17, Volatile: 3}
	result := reply{fileID: id}
	for _, test := range []struct {
		name    string
		request RequestContext
		input   wire.FileID
		want    wire.FileID
		status  smb.Status
	}{
		{"existing", RequestContext{related: true, fileID: id}, id, id, smb.StatusSuccess},
		{"inherited", RequestContext{related: true, fileID: result.fileID}, placeholder, id, smb.StatusSuccess},
		{"missing", RequestContext{related: true}, placeholder, wire.FileID{}, smb.StatusInvalidParameter},
		{"unrelated", RequestContext{fileID: id}, placeholder, placeholder, smb.StatusSuccess},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, status := test.request.FileID(test.input)
			if got != test.want || status != test.status {
				t.Fatalf("FileID: %+v, %v; want %+v, %v", got, status, test.want, test.status)
			}
		})
	}
}

func TestRequestBinding(t *testing.T) {
	request := RequestContext{Session: Session{SessionID: 17}, Tree: Tree{TreeID: 3}}
	if got := request.Binding(); got != (state.Binding{SessionID: 17, TreeID: 3}) {
		t.Fatalf("binding: %+v", got)
	}
}
