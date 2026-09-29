// SPDX-License-Identifier: AGPL-3.0-only
// Package smbserver composes the pinned SMB server with the native VFS adapter.
package smbserver

import (
	"context"
	"errors"
	"net"
	"strings"

	smb2 "github.com/djosh34/s3-smb/internal/smb2/server"
	"github.com/djosh34/s3-smb/internal/smb2/vfs"
)

type Server struct{ native *smb2.Server }

// New uses normal NTLM authentication even when password is explicitly empty.
// Configuration must distinguish an explicit empty password from missing input.
func New(share, account, password string, filesystem vfs.VFSFileSystem) (*Server, error) {
	if strings.TrimSpace(share) == "" || strings.ContainsAny(share, `/\\`) || strings.EqualFold(share, "IPC$") {
		return nil, errors.New("invalid SMB share name")
	}
	if strings.TrimSpace(account) == "" {
		return nil, errors.New("SMB account is required")
	}
	if filesystem == nil {
		return nil, errors.New("SMB filesystem is required")
	}
	auth := &smb2.NTLMAuthenticator{UserPassword: map[string]string{account: password}, NbName: "s3-smb"}
	return &Server{native: smb2.NewServer(&smb2.ServerConfig{Xatrrs: true}, auth, map[string]vfs.VFSFileSystem{share: filesystem})}, nil
}

// Serve owns listener until it returns. Bind policy belongs to configuration.
func (s *Server) Serve(listener net.Listener) error { return s.native.ServeListener(listener) }

// Shutdown stops accepts and waits for operations and handle cleanup. A deadline
// failure means callers must not close the filesystem or release its state lock.
func (s *Server) Shutdown(ctx context.Context) error { return s.native.ShutdownContext(ctx) }
