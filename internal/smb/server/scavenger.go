package server

import (
	"context"
	"time"

	"github.com/djosh34/s3-smb/internal/smb/state"
)

const scavengerInterval = time.Second

// startScavenger and stopScavenger run with server.mu held.
func (server *Server) startScavenger(ctx context.Context) {
	if server.scavengerStop != nil {
		return
	}
	server.scavengerStop = make(chan struct{})
	server.scavengerDone = make(chan struct{})
	go func() {
		ticker := time.NewTicker(scavengerInterval)
		defer ticker.Stop()
		server.runScavenger(context.WithoutCancel(ctx), ticker.C)
	}()
}

func (server *Server) stopScavenger() {
	if server.scavengerStop != nil {
		close(server.scavengerStop)
	}
}

func (server *Server) waitScavenger() {
	if server.scavengerDone != nil {
		<-server.scavengerDone
	}
}

func (server *Server) runScavenger(ctx context.Context, ticks <-chan time.Time) {
	defer close(server.scavengerDone)
	for {
		select {
		case <-server.scavengerStop:
			return
		case <-ticks:
			server.expire(ctx)
		}
	}
}

// expire uses the table's injected clock, not the ticker's wall-clock timestamp.
// Tests call it after advancing Options.Now without waiting for a tick.
func (server *Server) expire(ctx context.Context) {
	actions := server.options.State.Expire()
	actions = append(actions, server.options.State.ExpireBreaks()...)
	for _, action := range actions {
		if err := server.cleanup(ctx, []state.CloseAction{action}); err != nil {
			server.options.Logger.Error("expire open", "persistent_id", action.FileID.Persistent,
				"volatile_id", action.FileID.Volatile, "inode", action.Object.Inode,
				"stream", action.Object.Stream, "error", err)
		}
	}
}
