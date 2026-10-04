//go:build !smbnext

// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"context"
	"net"

	"github.com/djosh34/s3-smb/internal/config"
	smb2 "github.com/djosh34/s3-smb/internal/smb-old/smb2/server"
	"github.com/djosh34/s3-smb/internal/smb-old/smb2/vfs"
	"github.com/djosh34/s3-smb/internal/smb-old/smbfs"
	"github.com/djosh34/s3-smb/internal/storage"
)

type oldServer struct{ *smb2.Server }

func (s *oldServer) Serve(_ context.Context, listener net.Listener) error {
	return s.ServeListener(listener)
}

func (s *oldServer) Shutdown(ctx context.Context) error {
	return s.ShutdownContext(ctx)
}

func newSMBServer(runtime *storage.Runtime, c config.SMBConfig, _ string) (smbServer, smbAdapter, error) {
	adapter, err := smbfs.New(runtime.FS, c.ReadOnly)
	if err != nil {
		return nil, nil, err
	}
	auth := &smb2.NTLMAuthenticator{UserPassword: map[string]string{c.Username: c.Password}, NbName: "s3-smb"}
	s := smb2.NewServer(&smb2.ServerConfig{Xatrrs: true}, auth, map[string]vfs.VFSFileSystem{c.Share: adapter})
	return &oldServer{s}, adapter, nil
}
