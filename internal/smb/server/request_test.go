package server

import (
	"context"
	"testing"

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

func TestRequestBinding(t *testing.T) {
	request := RequestContext{Session: Session{SessionID: 17}, Tree: Tree{TreeID: 3}}
	if got := request.Binding(); got != (state.Binding{SessionID: 17, TreeID: 3}) {
		t.Fatalf("binding: %+v", got)
	}
}
