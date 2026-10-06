// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"crypto/rand"
	"log/slog"
	"time"

	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/server"
	"github.com/djosh34/s3-smb/internal/smb/state"
)

func newSMBServer(storage smb.Storage, c config.SMBConfig) (smbServer, error) {
	table, err := state.New(time.Now)
	if err != nil {
		return nil, err
	}
	policy := server.RequireEncryption
	if !c.Encryption {
		policy = server.AllowPlaintext
	}
	var guid [16]byte
	if _, err = rand.Read(guid[:]); err != nil {
		return nil, err
	}
	s, err := server.New(server.Options{
		Storage: storage, State: table, Logger: slog.Default(), Now: time.Now,
		Account:   auth.Account{User: c.Username, Password: c.Password},
		ShareName: c.Share, ServerName: "s3-smb", ServerGUID: guid, Encryption: policy,
	})
	if err != nil {
		// A nil *Server in the interface would not compare equal to nil.
		return nil, err
	}
	return s, nil
}
