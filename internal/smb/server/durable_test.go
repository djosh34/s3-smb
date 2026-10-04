package server

import (
	"bytes"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestDurableTimeoutsAndPersistence(t *testing.T) {
	for _, milliseconds := range []uint32{0, 1, 119999, 120000, 300000, 959999, 960000, 960001, ^uint32(0)} {
		t.Run((time.Duration(milliseconds) * time.Millisecond).String(), func(t *testing.T) {
			server, client, ctx, session := newFileClient(t)
			options := durableCreateOptions()
			options.Durable.Timeout = milliseconds
			options.Durable.Flags = 2 // Persistent requested, but never granted.
			result := durableResult(t, durableExchange(ctx, t, client, session, session.NextMessageID, options))
			want := min(milliseconds, uint32(smb.MaxDurableTimeout/time.Millisecond))
			if want == 0 {
				want = uint32(smb.DefaultDurableTimeout / time.Millisecond)
			}
			if result.Durable == nil || result.Durable.Timeout != want || result.Durable.Flags != 0 || result.Lease == nil || result.Lease.State&smb.LeaseHandle == 0 {
				t.Fatalf("durable grant = %+v, lease = %+v, want timeout %d", result.Durable, result.Lease, want)
			}
			open, status := server.options.State.Find(state.FileID(result.Reply.ID), state.Binding{SessionID: session.SessionID, TreeID: session.TreeID})
			if status != smb.StatusSuccess || !open.Durable || open.DurableTimeout != time.Duration(want)*time.Millisecond {
				t.Fatalf("durable state = %+v, status %#x", open, status)
			}
		})
	}
}

func TestDurableGrantRequiresUnnamedRegularFileAndH(t *testing.T) {
	for _, test := range []struct {
		modify func(*smbtest.CreateOptions)
		name   string
	}{
		{name: "no lease", modify: func(o *smbtest.CreateOptions) { o.Lease = nil }},
		{name: "no H", modify: func(o *smbtest.CreateOptions) { o.Lease.State = smb.LeaseRead }},
		{name: "directory", modify: func(o *smbtest.CreateOptions) { o.Request.Options = fileDirectoryFile }},
		{name: "stream", modify: func(o *smbtest.CreateOptions) { o.Request.Name = "base:attribute" }},
		{name: "durable v1", modify: func(o *smbtest.CreateOptions) {
			o.Durable = nil
			o.Request.Contexts = []wire.CreateContext{{Name: "DHnQ", Data: make([]byte, 16)}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, client, ctx, session := newFileClient(t)
			id := session.NextMessageID
			options := durableCreateOptions()
			test.modify(&options)
			if test.name == "stream" {
				createdFile(t, fileCreate(ctx, t, client, session, id, wire.CreateRequest{Name: "base", DesiredAccess: fileAllAccess, ShareAccess: 7, Disposition: fileOpenIf}))
				id++
			}
			result := durableResult(t, durableExchange(ctx, t, client, session, id, options))
			if result.Durable != nil {
				t.Fatalf("unsupported durable grant: %+v", result.Durable)
			}
			open, status := server.options.State.Find(state.FileID(result.Reply.ID), state.Binding{SessionID: session.SessionID, TreeID: session.TreeID})
			if status != smb.StatusSuccess || open.Durable {
				t.Fatalf("unsupported durable state: %+v, %#x", open, status)
			}
		})
	}
}

func TestDurableReplayNeverRepeatsMutations(t *testing.T) {
	server, client, ctx, session := newFileClient(t)
	options := durableCreateOptions()
	options.Request.Disposition = fileOverwriteIf
	id := session.NextMessageID
	first := durableResult(t, durableExchange(ctx, t, client, session, id, options))
	binding := state.Binding{SessionID: session.SessionID, TreeID: session.TreeID}
	open, status := server.options.State.Find(state.FileID(first.Reply.ID), binding)
	if status != smb.StatusSuccess {
		t.Fatalf("find = %#x", status)
	}
	data := []byte("acknowledged bytes must not be truncated by replay")
	if _, err := server.options.Storage.WriteAt(ctx, open.Handle, data, 0); err != nil {
		t.Fatal(err)
	}
	options.Replay = true
	replay := durableResult(t, durableExchange(ctx, t, client, session, id+1, options))
	if replay.Reply.ID != first.Reply.ID || replay.Reply.Action != first.Reply.Action || replay.Durable == nil || replay.Lease == nil || *replay.Lease != *first.Lease {
		t.Fatalf("replay changed grant: first %+v, replay %+v", first, replay)
	}
	buffer := make([]byte, len(data))
	if _, err := server.options.Storage.ReadAt(ctx, open.Handle, buffer, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buffer, data) {
		t.Fatalf("replay mutated bytes: %q", buffer)
	}
	options.Replay = false
	options.Request.Name = "must-not-be-created"
	duplicate := durableExchange(ctx, t, client, session, id+2, options)
	if duplicate.Header.Status != smb.StatusDuplicateObjectID {
		t.Fatalf("duplicate status = %#x", duplicate.Header.Status)
	}
	resolved, err := server.options.Storage.Lookup(ctx, options.Request.Name)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Exists {
		t.Fatal("duplicate created a different name")
	}
}

func TestDurableReplayRejectsChangedParameters(t *testing.T) {
	for _, test := range []struct {
		modify func(*smbtest.CreateOptions)
		name   string
	}{
		{name: "name", modify: func(o *smbtest.CreateOptions) { o.Request.Name = "other" }},
		{name: "access", modify: func(o *smbtest.CreateOptions) { o.Request.DesiredAccess = fileReadData }},
		{name: "sharing", modify: func(o *smbtest.CreateOptions) { o.Request.ShareAccess = 1 }},
		{name: "disposition", modify: func(o *smbtest.CreateOptions) { o.Request.Disposition = fileOverwrite }},
		{name: "options", modify: func(o *smbtest.CreateOptions) { o.Request.Options |= fileDeleteOnClose }},
		{name: "attributes", modify: func(o *smbtest.CreateOptions) { o.Request.FileAttributes = 2 }},
		{name: "impersonation", modify: func(o *smbtest.CreateOptions) { o.Request.ImpersonationLevel++ }},
		{name: "timeout", modify: func(o *smbtest.CreateOptions) { o.Durable.Timeout++ }},
		{name: "lease", modify: func(o *smbtest.CreateOptions) { o.Lease.Key[0]++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, client, ctx, session := newFileClient(t)
			options := durableCreateOptions()
			first := durableResult(t, durableExchange(ctx, t, client, session, session.NextMessageID, options))
			options.Replay = true
			test.modify(&options)
			message := durableExchange(ctx, t, client, session, session.NextMessageID+1, options)
			if message.Header.Status != smb.StatusInvalidParameter {
				t.Fatalf("changed %s replay status = %#x", test.name, message.Header.Status)
			}
			if _, status := server.options.State.Find(state.FileID(first.Reply.ID), state.Binding{SessionID: session.SessionID, TreeID: session.TreeID}); status != smb.StatusSuccess {
				t.Fatalf("rejected replay lost open: %#x", status)
			}
		})
	}
}
